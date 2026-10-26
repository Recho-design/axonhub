package orchestrator

import (
	"context"
	"sort"
	"time"

	"github.com/looplj/axonhub/internal/server/biz"
)

// ChannelModelMetricsProvider provides health metrics for one upstream model on a channel.
type ChannelModelMetricsProvider interface {
	GetChannelModelMetrics(ctx context.Context, channelID int, modelID string) (*biz.AggregatedMetrics, error)
}

// healthModelOf returns the model that will be attempted first for a candidate.
func healthModelOf(candidate *ChannelModelsCandidate) string {
	if candidate == nil || len(candidate.Models) == 0 {
		return ""
	}

	return candidate.Models[0].ActualModel
}

// candidateModelHealthRank is one model entry with the health signals used to
// order the candidate's retry list.
type candidateModelHealthRank struct {
	entry           biz.ChannelModelEntry
	apiFormat       string
	recentlyFailing bool
	failures        int64
	lastFailureAt   time.Time
	originalIndex   int
}

// orderCandidateModelsByHealth moves recently failing models behind healthy models
// while keeping the model-to-API-format relationship intact.
func orderCandidateModelsByHealth(
	ctx context.Context,
	provider ChannelModelMetricsProvider,
	candidates []*ChannelModelsCandidate,
) {
	if provider == nil {
		return
	}

	for _, candidate := range candidates {
		orderCandidateModelsByHealthForCandidate(ctx, provider, candidate)
	}
}

func orderCandidateModelsByHealthForCandidate(
	ctx context.Context,
	provider ChannelModelMetricsProvider,
	candidate *ChannelModelsCandidate,
) {
	if candidate == nil || candidate.Channel == nil || len(candidate.Models) < 2 ||
		len(candidate.modelAPIFormats) != len(candidate.Models) {
		return
	}

	ranked := rankCandidateModelsByHealth(ctx, provider, candidate)
	sort.SliceStable(ranked, func(i, j int) bool {
		return candidateModelHealthLess(ranked[i], ranked[j])
	})

	for i, model := range ranked {
		candidate.Models[i] = model.entry
		candidate.modelAPIFormats[i] = model.apiFormat
	}
	candidate.APIFormat = candidate.modelAPIFormats[0]
}

// rankCandidateModelsByHealth queries model health for every entry in the
// candidate's retry list. Query errors and nil metrics leave the entry healthy
// so an unavailable metrics source never reorders models by itself.
func rankCandidateModelsByHealth(
	ctx context.Context,
	provider ChannelModelMetricsProvider,
	candidate *ChannelModelsCandidate,
) []candidateModelHealthRank {
	ranked := make([]candidateModelHealthRank, len(candidate.Models))
	for i, entry := range candidate.Models {
		model := candidateModelHealthRank{
			entry:         entry,
			apiFormat:     candidate.modelAPIFormats[i],
			originalIndex: i,
		}

		metrics, err := provider.GetChannelModelMetrics(ctx, candidate.Channel.ID, entry.ActualModel)
		if err == nil && metrics != nil {
			model.failures = metrics.ConsecutiveFailures
			if metrics.LastFailureAt != nil {
				model.lastFailureAt = *metrics.LastFailureAt
			}
			// Persisted failures always carry a timestamp; the nil case only
			// covers drained or malformed in-memory data and is treated as recent.
			model.recentlyFailing = metrics.ConsecutiveFailures > 0 &&
				(metrics.LastFailureAt == nil || time.Since(*metrics.LastFailureAt) < roundRobinHealthCooldown)
		}

		ranked[i] = model
	}

	return ranked
}

// candidateModelHealthLess orders healthy models first, then failing models by
// failure count, oldest failure time, and original position.
func candidateModelHealthLess(left, right candidateModelHealthRank) bool {
	if left.recentlyFailing != right.recentlyFailing {
		return !left.recentlyFailing
	}

	if !left.recentlyFailing {
		return left.originalIndex < right.originalIndex
	}

	if left.failures != right.failures {
		return left.failures < right.failures
	}
	if !left.lastFailureAt.Equal(right.lastFailureAt) {
		return left.lastFailureAt.Before(right.lastFailureAt)
	}

	return left.originalIndex < right.originalIndex
}

// lookupModelMetrics looks up model-level metrics when the provider supports them.
// Errors and nil metrics are treated as unavailable model metrics.
func lookupModelMetrics(
	ctx context.Context,
	provider ChannelMetricsProvider,
	channelID int,
	modelID string,
) (*biz.AggregatedMetrics, bool) {
	if provider == nil || modelID == "" {
		return nil, false
	}

	modelProvider, ok := provider.(ChannelModelMetricsProvider)
	if !ok {
		return nil, false
	}

	metrics, err := modelProvider.GetChannelModelMetrics(ctx, channelID, modelID)
	if err != nil || metrics == nil {
		return nil, false
	}

	return metrics, true
}

// modelMetricsFromContext returns model metrics for the candidate model carried
// in the per-candidate load-balancer context.
func modelMetricsFromContext(
	ctx context.Context,
	provider ChannelMetricsProvider,
	channelID int,
) (*biz.AggregatedMetrics, bool) {
	return lookupModelMetrics(ctx, provider, channelID, candidateModelFromContext(ctx))
}
