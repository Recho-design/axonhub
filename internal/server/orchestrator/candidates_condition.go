package orchestrator

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
)

// filterResolvedCandidatesForRequest evaluates `When` only after association
// resolution. This keeps the matching step focused on static association
// structure and makes request-dependent filtering easier to follow.
func filterResolvedCandidatesForRequest(
	ctx context.Context,
	req *llm.Request,
	resolvedCandidates []*resolvedAssociationCandidate,
) []*ChannelModelsCandidate {
	if len(resolvedCandidates) == 0 {
		return []*ChannelModelsCandidate{}
	}

	hasConditionalCandidates := lo.ContainsBy(resolvedCandidates, func(candidate *resolvedAssociationCandidate) bool {
		return candidate != nil && candidate.when != nil
	})
	if !hasConditionalCandidates {
		candidates := aggregateChannelModelCandidates(resolvedCandidates)
		candidates = populateAPIFormat(ctx, candidates, req)

		return candidates
	}

	promptTokens := estimatePromptTokens(req)
	stream := reqStream(req)
	requestFormat := reqAPIFormat(req)
	reasoningEffort := reqReasoningEffort(req)
	contentFeatures := detectRequestContentFeatures(req)
	requestHeaders := buildRequestHeaderMap(req)
	messagesText := extractRequestMessagesText(req)
	now := time.Now()
	filtered := make([]*resolvedAssociationCandidate, 0, len(resolvedCandidates))

	for _, candidate := range resolvedCandidates {
		if candidate == nil {
			continue
		}

		if !matchesAssociationWhen(promptTokens, stream, requestFormat, reasoningEffort, contentFeatures, requestHeaders, messagesText, now, candidate.when) {
			continue
		}

		filtered = append(filtered, candidate)
	}

	candidates := aggregateChannelModelCandidates(filtered)

	if log.DebugEnabled(ctx) {
		log.Debug(ctx, "evaluated conditional associations",
			log.Int64("estimated_prompt_tokens", promptTokens),
			log.Int("matched_candidate_count", len(filtered)),
		)
	}

	candidates = populateAPIFormat(ctx, candidates, req)

	return candidates
}

func populateAPIFormat(ctx context.Context, candidates []*ChannelModelsCandidate, req *llm.Request) []*ChannelModelsCandidate {
	filtered := make([]*ChannelModelsCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c == nil || c.Channel == nil {
			continue
		}

		// A candidate may contain several models that are retried in order. Apply
		// protocol overrides to one model at a time; applying them to the whole
		// slice would merge unrelated model overrides and select the wrong
		// protocol for the first attempt.
		baseEndpoints := c.Channel.ResolveEndpoints()
		if len(c.Models) > 0 {
			selectedModels := make([]biz.ChannelModelEntry, 0, len(c.Models))
			selectedFormats := make([]string, 0, len(c.Models))
			for _, entry := range c.Models {
				endpoints := applyForcedAPIFormatsForRequest(ctx, c.Channel, []biz.ChannelModelEntry{entry}, req.Model, req.RequestType, baseEndpoints)
				format := SelectAPIFormat(endpoints, req)
				// Explicit protocols have no generic fallback. A model whose forced protocol
				// list cannot serve the requested protocol must not remain as the first retry
				// entry, otherwise an empty candidate format falls back to the channel's
				// primary (usually chat) outbound.
				if requiresExplicitEndpoint(req.RequestType) && format == "" {
					continue
				}

				selectedModels = append(selectedModels, entry)
				selectedFormats = append(selectedFormats, format)
			}

			if len(selectedModels) == 0 {
				continue
			}

			if requiresExplicitEndpoint(req.RequestType) {
				c.Models = selectedModels
			}
			c.modelAPIFormats = selectedFormats
			c.APIFormat = selectedFormats[0]
		} else {
			c.modelAPIFormats = nil
			endpoints := applyForcedAPIFormatsForRequest(ctx, c.Channel, c.Models, req.Model, req.RequestType, baseEndpoints)
			c.APIFormat = SelectAPIFormat(endpoints, req)
		}

		if requiresExplicitEndpoint(req.RequestType) && c.APIFormat == "" {
			continue
		}

		filtered = append(filtered, c)
	}

	return filtered
}

func reqStream(req *llm.Request) bool {
	if req == nil || req.Stream == nil {
		return false
	}

	return *req.Stream
}

func reqAPIFormat(req *llm.Request) string {
	if req == nil {
		return ""
	}

	return string(req.APIFormat)
}

func reqReasoningEffort(req *llm.Request) string {
	if req == nil {
		return ""
	}

	return req.ReasoningEffort
}

type requestContentFeatures struct {
	hasImage    bool
	hasVideo    bool
	hasDocument bool
	hasAudio    bool
}

func matchesAssociationWhen(
	promptTokens int64,
	stream bool,
	requestFormat string,
	reasoningEffort string,
	contentFeatures requestContentFeatures,
	requestHeaders map[string]string,
	messagesText string,
	now time.Time,
	when *objects.ModelAssociationWhen,
) bool {
	if when == nil {
		return true
	}

	if !when.Enabled {
		return true
	}

	if when.Condition != nil && !objects.Evaluate(*when.Condition, map[string]any{
		objects.ModelAssociationConditionFieldPromptTokens:    promptTokens,
		objects.ModelAssociationConditionFieldStream:          stream,
		objects.ModelAssociationConditionFieldRequestFormat:   requestFormat,
		objects.ModelAssociationConditionFieldReasoningEffort: reasoningEffort,
		objects.ModelAssociationConditionFieldHasImage:        contentFeatures.hasImage,
		objects.ModelAssociationConditionFieldHasVideo:        contentFeatures.hasVideo,
		objects.ModelAssociationConditionFieldHasDocument:     contentFeatures.hasDocument,
		objects.ModelAssociationConditionFieldHasAudio:        contentFeatures.hasAudio,
		objects.ModelAssociationConditionFieldRequestHeader:   requestHeaders,
		objects.ModelAssociationConditionFieldMessagesText:    messagesText,
		"now": now,
	}) {
		return false
	}

	return true
}

func detectRequestContentFeatures(req *llm.Request) requestContentFeatures {
	var features requestContentFeatures
	if req == nil {
		return features
	}

	for _, message := range req.Messages {
		for _, part := range message.Content.MultipleContent {
			switch {
			case part.ImageURL != nil:
				features.hasImage = true
			case part.VideoURL != nil:
				features.hasVideo = true
			case part.Document != nil:
				features.hasDocument = true
			case part.InputAudio != nil:
				features.hasAudio = true
			}

			if features.hasAll() {
				return features
			}
		}
	}

	return features
}

func (features requestContentFeatures) hasAll() bool {
	return features.hasImage && features.hasVideo && features.hasDocument && features.hasAudio
}

// requestMessagesTextMaxBytes caps the concatenated message text exposed to
// `messages_text` conditions. A single request can carry megabytes of text, and
// every conditional association runs a user supplied regex against this string,
// so the cap bounds the cost per request. 1 MiB covers a full conversation
// (roughly 350k Chinese characters) beyond most model context limits, and a
// well-behaved pattern still matches it in milliseconds. Text beyond the cap
// is ignored, so a pattern only matching the discarded tail will not hit.
const requestMessagesTextMaxBytes = 1024 * 1024

/**
 * extractRequestMessagesText concatenates the text content of all request
 * messages for the `messages_text` condition. Only text is collected: a plain
 * string content, or the `text` parts of a multi-part content array. Image,
 * audio, video and document payloads are skipped, so base64 data never reaches
 * the regex. Tool calls and reasoning content are skipped as well, since they
 * are model-generated data rather than the request prompt; the text of `tool`
 * role messages is included because it is ordinary message content.
 *
 * Messages are joined with a newline to keep the boundary between two messages
 * from forming an accidental match, and the result is capped at
 * requestMessagesTextMaxBytes.
 */
func extractRequestMessagesText(req *llm.Request) string {
	if req == nil || len(req.Messages) == 0 {
		return ""
	}

	var builder strings.Builder

	for _, message := range req.Messages {
		appendMessageText(&builder, message.Content)
		if builder.Len() >= requestMessagesTextMaxBytes {
			break
		}
	}

	return builder.String()
}

/**
 * appendMessageText appends the text of one message content to builder, stopping
 * at the shared byte budget. Content and MultipleContent are mutually exclusive
 * representations of the same payload, and llm.MessageContent marshals
 * MultipleContent when both are set; the same precedence is used here so the
 * matched text matches what is actually sent upstream.
 */
func appendMessageText(builder *strings.Builder, content llm.MessageContent) {
	if len(content.MultipleContent) == 0 {
		if content.Content != nil {
			appendTextWithinBudget(builder, *content.Content)
		}

		return
	}

	for _, part := range content.MultipleContent {
		// Only text parts are prompt text; the other part types carry media.
		if !strings.EqualFold(part.Type, "text") || part.Text == nil {
			continue
		}

		appendTextWithinBudget(builder, *part.Text)
	}
}

/**
 * appendTextWithinBudget appends text plus a separating newline to builder,
 * truncating to keep the total within requestMessagesTextMaxBytes.
 */
func appendTextWithinBudget(builder *strings.Builder, text string) {
	if text == "" {
		return
	}

	remaining := requestMessagesTextMaxBytes - builder.Len()

	if builder.Len() > 0 {
		// Reserve one byte for the separator written below.
		remaining--
		if remaining <= 0 {
			return
		}

		builder.WriteByte('\n')
	}

	if len(text) > remaining {
		text = truncateAtRuneBoundary(text, remaining)
	}

	builder.WriteString(text)
}

/**
 * truncateAtRuneBoundary cuts text to at most limit bytes without splitting a
 * multi-byte rune, so the matched text stays valid UTF-8.
 */
func truncateAtRuneBoundary(text string, limit int) string {
	if len(text) <= limit {
		return text
	}

	if limit <= 0 {
		return ""
	}

	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}

	return text[:limit]
}

func estimatePromptTokens(req *llm.Request) int64 {
	if req == nil {
		return 0
	}

	total := 0
	for _, message := range req.Messages {
		total += estimateTokens(message.Role)
		total += estimateTokensPtr(message.Name)
		total += countPromptMessageContent(message.Content)
		total += estimateTokensPtr(message.ToolCallID)
		total += estimateTokensPtr(message.ToolCallName)
		total += estimateTokensPtr(message.ReasoningContent)

		for _, toolCall := range message.ToolCalls {
			total += estimateTokens(toolCall.Type)
			total += estimateTokens(toolCall.Function.Name)
			total += estimateTokens(toolCall.Function.Arguments)
		}
	}

	total += countPromptTools(req.Tools)

	return int64(total)
}

func countPromptMessageContent(content llm.MessageContent) int {
	tokens := estimateTokensPtr(content.Content)
	for _, part := range content.MultipleContent {
		tokens += estimateTokens(part.Type)
		tokens += estimateTokensPtr(part.Text)

		if part.ImageURL != nil {
			tokens += 128
			tokens += estimateTokensPtr(part.ImageURL.Detail)
		}

		if part.VideoURL != nil {
			tokens += 128
		}

		if part.Document != nil {
			tokens += min(estimateTokens(part.Document.URL), 512)
			tokens += estimateTokens(part.Document.MIMEType)
		}

		if part.InputAudio != nil {
			tokens += 128
			tokens += estimateTokens(part.InputAudio.Format)
		}

		if part.Compact != nil {
			tokens += min(estimateTokens(part.Compact.EncryptedContent), 256)
		}
	}

	return tokens
}

func countPromptTools(tools []llm.Tool) int {
	if len(tools) == 0 {
		return 0
	}

	total := 0
	for _, tool := range tools {
		total += estimateTokens(tool.Type)

		total += estimateTokens(tool.Function.Name)
		total += estimateTokens(tool.Function.Description)
		total += estimateTokens(string(tool.Function.Parameters))

		if tool.WebSearch != nil {
			if tool.WebSearch.MaxUses != nil {
				total += 2
			}

			if tool.WebSearch.Strict != nil {
				total++
			}

			for _, domain := range tool.WebSearch.AllowedDomains {
				total += estimateTokens(domain)
			}

			for _, domain := range tool.WebSearch.BlockedDomains {
				total += estimateTokens(domain)
			}

			total += estimateTokens(tool.WebSearch.UserLocation.City)
			total += estimateTokens(tool.WebSearch.UserLocation.Country)
			total += estimateTokens(tool.WebSearch.UserLocation.Region)
			total += estimateTokens(tool.WebSearch.UserLocation.Timezone)
			total += estimateTokens(tool.WebSearch.UserLocation.Type)
		}

		if tool.ResponseCustomTool != nil {
			total += estimateTokens(tool.ResponseCustomTool.Name)

			total += estimateTokens(tool.ResponseCustomTool.Description)
			if tool.ResponseCustomTool.Format != nil {
				total += estimateTokens(tool.ResponseCustomTool.Format.Type)
				total += estimateTokens(tool.ResponseCustomTool.Format.Syntax)
				total += estimateTokens(tool.ResponseCustomTool.Format.Definition)
			}
		}
	}

	return total
}

func estimateTokens(value string) int {
	if len(value) == 0 {
		return 0
	}

	var (
		cjkChars   int
		otherChars int
	)

	for _, r := range value {
		if isCJK(r) {
			cjkChars++
			continue
		}

		if unicode.IsSpace(r) {
			continue
		}

		otherChars++
	}

	cjkTokens := float64(cjkChars) / 1.5
	otherTokens := float64(otherChars) / 4.0

	total := int(cjkTokens + otherTokens)
	if total == 0 {
		return 1
	}

	return total
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

func estimateTokensPtr(value *string) int {
	if value == nil {
		return 0
	}

	return estimateTokens(*value)
}
