package orchestrator

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

// contentErrorMatchers caches compiled content error matchers keyed by their
// pattern configuration. Patterns only change when an admin saves the retry
// policy, so each configuration is compiled once instead of once per request.
var contentErrorMatchers sync.Map

// contentErrorMatcherFor returns the matcher for the current content error
// detection config, or nil when the detection is disabled or has no usable
// pattern.
func contentErrorMatcherFor(detection biz.ContentErrorDetection) *pipeline.ContentErrorMatcher {
	if !detection.Enabled || len(detection.Patterns) == 0 {
		return nil
	}

	key := contentErrorMatcherKey(detection.Patterns)
	if cached, ok := contentErrorMatchers.Load(key); ok {
		if matcher, ok := cached.(*pipeline.ContentErrorMatcher); ok {
			return matcher
		}
	}

	matcher := pipeline.NewContentErrorMatcher(lo.Map(detection.Patterns, func(pattern objects.RetryableErrorPattern, _ int) pipeline.ContentErrorPattern {
		return pipeline.ContentErrorPattern{Pattern: pattern.Pattern, Regex: pattern.Regex}
	}))
	if matcher == nil {
		return nil
	}

	contentErrorMatchers.Store(key, matcher)

	return matcher
}

// contentErrorMatcherKey builds the cache key for a pattern list. Patterns are
// part of the key, so saving a new list compiles a new matcher.
func contentErrorMatcherKey(patterns []objects.RetryableErrorPattern) string {
	var builder strings.Builder
	for _, pattern := range patterns {
		builder.WriteString(pattern.Pattern)
		builder.WriteByte(0)

		if pattern.Regex {
			builder.WriteByte('1')
		} else {
			builder.WriteByte('0')
		}
	}

	return builder.String()
}

// contentErrorDetectionMiddleware rejects a non-streaming response whose
// delivered body contains upstream error text. The pass-through middleware
// replaces the transformed response with the raw provider body, so the pipeline
// content check (which runs on the transformed response) may not see the bytes
// the caller receives; scanning the buffered body here closes that gap.
func contentErrorDetectionMiddleware(
	outbound *PersistentOutboundTransformer,
	systemService *biz.SystemService,
	matcher *pipeline.ContentErrorMatcher,
) pipeline.Middleware {
	return pipeline.OnInboundRawResponse("pass-through-content-error-detection", func(ctx context.Context, response *httpclient.Response) (*httpclient.Response, error) {
		if matcher == nil || response == nil || !outbound.passThroughResponseEnabled(ctx, systemService) {
			return response, nil
		}

		match := matcher.DetectContentErrorInBody(response.Body)
		if match == nil {
			return response, nil
		}

		channel := outbound.GetCurrentChannel()
		log.Warn(ctx, "pass-through response content matched an error pattern",
			log.String("channel", channel.Name),
			log.Int("channel_id", channel.ID),
			log.String("pattern", match.Pattern),
			log.Bool("regex", match.Regex),
			log.String("content_preview", match.ContentPreview()),
		)

		return nil, pipeline.WrapContentErrorMatch(match)
	})
}

// streamContentErrorMessage builds the persisted failure reason for a stream
// whose aggregated content matched an error pattern. The matched content is
// deliberately excluded: it is already delivered to the client and belongs to
// the response body, not to the request record.
func streamContentErrorMessage(match *pipeline.ContentErrorMatchError) string {
	return fmt.Sprintf(
		"upstream streamed a 200 response containing error content (matched pattern: %q)",
		match.Pattern,
	)
}

// contentErrorDetected reports whether stream finalization already matched the
// aggregated response body against the configured patterns.
func (ts *OutboundPersistentStream) contentErrorDetected() bool {
	return ts.state != nil && ts.state.ContentErrorMatched
}

// detectStreamContentError scans the aggregated stream body for configured error
// patterns. A match fails the attempt for persistence and channel health without
// touching the bytes already delivered and without scheduling a retry: streaming
// retries only happen before the first event is committed, and a committed
// attempt must not restart after the client saw the content. Automatic channel
// disabling is intentionally left to the normal failure policy of the request.
func (ts *OutboundPersistentStream) detectStreamContentError(ctx context.Context, body []byte) {
	if ts.contentErrorMatcher == nil || ts.state == nil || len(body) == 0 || ts.contentErrorDetected() {
		return
	}

	match := ts.contentErrorMatcher.DetectContentErrorInBody(body)
	if match == nil {
		return
	}

	fields := []log.Field{
		log.String("pattern", match.Pattern),
		log.Bool("regex", match.Regex),
		log.String("content_preview", match.ContentPreview()),
	}
	if channel := ts.currentChannel(); channel != nil {
		fields = append(fields, log.String("channel", channel.Name), log.Int("channel_id", channel.ID))
	}
	log.Warn(ctx, "stream response content matched an error pattern", fields...)

	message := streamContentErrorMessage(match)
	ts.terminalError = message
	ts.state.ContentErrorMatched = true
	ts.state.ContentErrorMessage = message

	// The channel answered 200 and streamed error text as content, which is the
	// same channel fault the non-streaming detection reports. Marking the record
	// failed (instead of the success marked by the terminal event) is what the
	// error-aware and round-robin health strategies observe.
	if ts.perf != nil {
		ts.perf.MarkFailedWithMessage(http.StatusBadGateway, message)
	}
}

// currentChannel returns the channel of the attempt being finalized.
func (ts *OutboundPersistentStream) currentChannel() *biz.Channel {
	if ts.state == nil || ts.state.CurrentCandidate == nil {
		return nil
	}

	return ts.state.CurrentCandidate.Channel
}
