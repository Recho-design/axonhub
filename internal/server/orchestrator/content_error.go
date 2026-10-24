package orchestrator

import (
	"context"
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
