package pipeline

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
)

// ErrContentErrorMatch reports that a successful non-streaming response carried
// upstream error text inside the assistant content. Some providers answer with
// HTTP 200 and a textual error instead of an error status, so the response must
// not be delivered and the attempt counts as failed for the retry policy.
var ErrContentErrorMatch = errors.New("response content matched a configured error pattern")

const (
	// contentErrorPreviewLimit bounds the content excerpt kept for operator logs.
	contentErrorPreviewLimit = 200
	// contentErrorPreviewContext keeps some text before the match so operators can
	// read the surrounding phrasing.
	contentErrorPreviewContext = 40
)

// ContentErrorPattern is one content matcher. When Regex is false, Pattern is
// matched as a case-sensitive substring of the assistant content.
type ContentErrorPattern struct {
	Pattern string
	Regex   bool
}

// ContentErrorMatchError describes one matched pattern. It never exposes the
// response content through Error(): API users only see the pattern, while
// operators can read the bounded preview from logs.
type ContentErrorMatchError struct {
	// Pattern is the configured pattern that matched the response content.
	Pattern string

	// Regex reports whether Pattern is a regular expression.
	Regex bool

	// preview keeps a bounded excerpt of the matched content for operator logs.
	preview string
}

// Error implements error. The matched response content is deliberately excluded
// so the error can be returned to API users and stored on request records.
func (e *ContentErrorMatchError) Error() string {
	if e == nil {
		return ErrContentErrorMatch.Error()
	}

	return fmt.Sprintf("%s: pattern %q", ErrContentErrorMatch.Error(), e.Pattern)
}

// Unwrap lets callers match the error with errors.Is(err, ErrContentErrorMatch).
func (e *ContentErrorMatchError) Unwrap() error {
	return ErrContentErrorMatch
}

// ContentPreview returns the bounded content excerpt kept for operator logs.
func (e *ContentErrorMatchError) ContentPreview() string {
	if e == nil {
		return ""
	}

	return e.preview
}

// ContentErrorMatcher matches assistant content against configured patterns.
// Regexes are compiled once when the matcher is built, so a matcher must be
// built once per policy change and reused across requests.
type ContentErrorMatcher struct {
	patterns []compiledContentErrorPattern
}

type compiledContentErrorPattern struct {
	pattern ContentErrorPattern
	regexp  *regexp.Regexp
}

// NewContentErrorMatcher builds a matcher from the configured patterns. Empty
// patterns are dropped, and so is an invalid regular expression: the policy save
// path rejects those, and a legacy invalid value must not fail every request.
// It returns nil when no pattern is usable.
func NewContentErrorMatcher(patterns []ContentErrorPattern) *ContentErrorMatcher {
	compiled := make([]compiledContentErrorPattern, 0, len(patterns))

	for _, pattern := range patterns {
		pattern.Pattern = strings.TrimSpace(pattern.Pattern)
		if pattern.Pattern == "" {
			continue
		}

		entry := compiledContentErrorPattern{pattern: pattern}
		if pattern.Regex {
			re, err := regexp.Compile(pattern.Pattern)
			if err != nil {
				continue
			}

			entry.regexp = re
		}

		compiled = append(compiled, entry)
	}

	if len(compiled) == 0 {
		return nil
	}

	return &ContentErrorMatcher{patterns: compiled}
}

// empty reports whether the matcher has no usable pattern.
func (m *ContentErrorMatcher) empty() bool {
	return m == nil || len(m.patterns) == 0
}

// MatchContent returns the first pattern matched by text, or nil.
func (m *ContentErrorMatcher) MatchContent(text string) *ContentErrorMatchError {
	if m.empty() || text == "" {
		return nil
	}

	for _, entry := range m.patterns {
		offset, matched := entry.matchOffset(text)
		if !matched {
			continue
		}

		return &ContentErrorMatchError{
			Pattern: entry.pattern.Pattern,
			Regex:   entry.pattern.Regex,
			preview: contentErrorPreview(text, offset),
		}
	}

	return nil
}

// matchOffset reports where pattern matched text, so the preview can start near
// the match instead of at the beginning of a long answer.
func (e compiledContentErrorPattern) matchOffset(text string) (int, bool) {
	if e.regexp != nil {
		loc := e.regexp.FindStringIndex(text)
		if loc == nil {
			return 0, false
		}

		return loc[0], true
	}

	offset := strings.Index(text, e.pattern.Pattern)
	if offset < 0 {
		return 0, false
	}

	return offset, true
}

// DetectContentError returns the first match in the assistant text of a
// non-streaming response, or nil. Only choices[].message content is scanned:
// tool calls, reasoning, embedding, audio and completion text are ignored
// because providers deliver textual errors as chat content.
func (m *ContentErrorMatcher) DetectContentError(resp *llm.Response) *ContentErrorMatchError {
	if m.empty() || resp == nil {
		return nil
	}

	for _, choice := range resp.Choices {
		for _, text := range messageTexts(choice.Message) {
			if match := m.MatchContent(text); match != nil {
				return match
			}
		}
	}

	return nil
}

// DetectContentErrorInBody returns the first match in a buffered non-streaming
// JSON response body, or nil. It backs the detection for pass-through responses,
// where the buffered provider body is delivered instead of the transformed one.
// Both the OpenAI shape (choices[].message.content) and the Anthropic shape
// (top-level content) are scanned, because pass-through delivers whichever
// format the caller requested. Missing fields and malformed bodies simply skip
// detection.
func (m *ContentErrorMatcher) DetectContentErrorInBody(body []byte) *ContentErrorMatchError {
	if m.empty() || len(body) == 0 {
		return nil
	}

	for _, choice := range gjson.GetBytes(body, "choices").Array() {
		for _, text := range jsonContentTexts(choice.Get("message.content")) {
			if match := m.MatchContent(text); match != nil {
				return match
			}
		}
	}

	// Anthropic Messages shape: top-level content as text blocks or a plain string.
	for _, text := range jsonContentTexts(gjson.GetBytes(body, "content")) {
		if match := m.MatchContent(text); match != nil {
			return match
		}
	}

	return nil
}

// WrapContentErrorMatch builds the error returned when content error detection
// rejects a response. It is a 502 upstream error so the existing upstream error
// policy (passthrough/hidden/custom) can rewrite the message, and its message
// never contains the matched response content.
func WrapContentErrorMatch(match *ContentErrorMatchError) error {
	if match == nil {
		return nil
	}

	return WrapUpstreamError(&llm.ResponseError{
		StatusCode: http.StatusBadGateway,
		Cause:      match,
		Detail: llm.ErrorDetail{
			Message: fmt.Sprintf(
				"Upstream returned a 200 response containing error content (matched pattern: %q)",
				match.Pattern,
			),
			Type: "upstream_error",
		},
	})
}

// messageTexts returns the assistant text of an llm message: the plain content
// and the text parts of a multi-part content. Both representations are scanned
// because a transformer may fill either of them for the same payload.
func messageTexts(msg *llm.Message) []string {
	if msg == nil {
		return nil
	}

	texts := make([]string, 0, 1+len(msg.Content.MultipleContent))
	if msg.Content.Content != nil && *msg.Content.Content != "" {
		texts = append(texts, *msg.Content.Content)
	}

	for _, part := range msg.Content.MultipleContent {
		if part.Type == "text" && part.Text != nil && *part.Text != "" {
			texts = append(texts, *part.Text)
		}
	}

	return texts
}

// jsonContentTexts returns the text carried by an OpenAI-style message content
// value, either a plain string or an array of text parts. Other part types and
// unexpected JSON shapes are ignored.
func jsonContentTexts(content gjson.Result) []string {
	switch {
	case content.Type == gjson.String:
		if text := content.String(); text != "" {
			return []string{text}
		}

		return nil
	case content.IsArray():
		texts := make([]string, 0, len(content.Array()))
		for _, part := range content.Array() {
			if part.Get("type").String() != "text" {
				continue
			}

			if text := part.Get("text").String(); text != "" {
				texts = append(texts, text)
			}
		}

		return texts
	default:
		return nil
	}
}

// contentErrorPreview returns a bounded excerpt of text starting near offset.
func contentErrorPreview(text string, offset int) string {
	if offset <= 0 || offset > len(text) {
		offset = 0
	}

	start := max(offset-contentErrorPreviewContext, 0)
	// Keep the excerpt valid UTF-8 even when the context window splits a rune.
	for start > 0 && start < len(text) && !utf8.RuneStart(text[start]) {
		start--
	}

	runes := []rune(text[start:])
	if len(runes) > contentErrorPreviewLimit {
		runes = runes[:contentErrorPreviewLimit]
	}

	return string(runes)
}
