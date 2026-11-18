package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/server/biz"
)

type modelHealthKey struct {
	channelID int
	modelID   string
}

type modelAwareMetricsProvider struct {
	channelMetrics map[int]*biz.AggregatedMetrics
	modelMetrics   map[modelHealthKey]*biz.AggregatedMetrics
	modelErrors    map[modelHealthKey]error
}

func (p *modelAwareMetricsProvider) GetChannelMetrics(_ context.Context, channelID int) (*biz.AggregatedMetrics, error) {
	if metrics, ok := p.channelMetrics[channelID]; ok {
		return metrics, nil
	}

	return &biz.AggregatedMetrics{}, nil
}

func (p *modelAwareMetricsProvider) GetChannelModelMetrics(_ context.Context, channelID int, modelID string) (*biz.AggregatedMetrics, error) {
	key := modelHealthKey{channelID: channelID, modelID: modelID}
	if err, ok := p.modelErrors[key]; ok {
		return nil, err
	}
	if metrics, ok := p.modelMetrics[key]; ok {
		return metrics, nil
	}

	return &biz.AggregatedMetrics{}, nil
}

func modelHealthMetrics(consecutiveFailures int64, lastFailureAt *time.Time) *biz.AggregatedMetrics {
	metrics := &biz.AggregatedMetrics{}
	metrics.ConsecutiveFailures = consecutiveFailures
	metrics.LastFailureAt = lastFailureAt

	return metrics
}

func modelHealthCandidate(channelID int, models ...string) *ChannelModelsCandidate {
	entries := make([]biz.ChannelModelEntry, 0, len(models))
	formats := make([]string, 0, len(models))
	for _, model := range models {
		entries = append(entries, biz.ChannelModelEntry{ActualModel: model})
		formats = append(formats, "openai/chat")
	}

	return &ChannelModelsCandidate{
		Channel:         &biz.Channel{Channel: &ent.Channel{ID: channelID}},
		Models:          entries,
		modelAPIFormats: formats,
	}
}

func TestOrderCandidateModelsByHealthKeepsFormatsInSync(t *testing.T) {
	provider := &modelAwareMetricsProvider{
		modelMetrics: map[modelHealthKey]*biz.AggregatedMetrics{
			{channelID: 1, modelID: "gptb"}: modelHealthMetrics(1, nil),
			{channelID: 1, modelID: "gpta"}: {},
		},
	}
	candidate := modelHealthCandidate(1, "gptb", "gpta")
	candidate.modelAPIFormats = []string{"format-b", "format-a"}

	orderCandidateModelsByHealth(context.Background(), provider, []*ChannelModelsCandidate{candidate})

	require.Equal(t, []string{"gpta", "gptb"}, []string{candidate.Models[0].ActualModel, candidate.Models[1].ActualModel})
	assert.Equal(t, []string{"format-a", "format-b"}, candidate.modelAPIFormats)
}

func TestOrderCandidateModelsByHealthSortsFailuresByCountAndTime(t *testing.T) {
	now := time.Now()
	early := now.Add(-3 * time.Minute)
	late := now.Add(-time.Minute)
	provider := &modelAwareMetricsProvider{
		modelMetrics: map[modelHealthKey]*biz.AggregatedMetrics{
			{channelID: 1, modelID: "many"}:      modelHealthMetrics(3, &late),
			{channelID: 1, modelID: "few-late"}:  modelHealthMetrics(1, &late),
			{channelID: 1, modelID: "few-early"}: modelHealthMetrics(1, &early),
		},
	}
	candidate := modelHealthCandidate(1, "many", "few-late", "few-early")

	orderCandidateModelsByHealth(context.Background(), provider, []*ChannelModelsCandidate{candidate})

	assert.Equal(t, []string{"few-early", "few-late", "many"}, []string{
		candidate.Models[0].ActualModel,
		candidate.Models[1].ActualModel,
		candidate.Models[2].ActualModel,
	})
}

func TestOrderCandidateModelsByHealthKeepsUnknownMetricsOrder(t *testing.T) {
	provider := &modelAwareMetricsProvider{
		modelErrors: map[modelHealthKey]error{
			{channelID: 1, modelID: "model-a"}: errors.New("metrics unavailable"),
			{channelID: 1, modelID: "model-b"}: errors.New("metrics unavailable"),
			{channelID: 1, modelID: "model-c"}: errors.New("metrics unavailable"),
		},
	}
	candidate := modelHealthCandidate(1, "model-a", "model-b", "model-c")
	originalFormats := append([]string(nil), candidate.modelAPIFormats...)

	orderCandidateModelsByHealth(context.Background(), provider, []*ChannelModelsCandidate{candidate})

	assert.Equal(t, []string{"model-a", "model-b", "model-c"}, []string{
		candidate.Models[0].ActualModel,
		candidate.Models[1].ActualModel,
		candidate.Models[2].ActualModel,
	})
	assert.Equal(t, originalFormats, candidate.modelAPIFormats)
}

func TestOrderCandidateModelsByHealthSkipsMismatchedFormats(t *testing.T) {
	provider := &modelAwareMetricsProvider{
		modelMetrics: map[modelHealthKey]*biz.AggregatedMetrics{
			{channelID: 1, modelID: "failed"}:  modelHealthMetrics(1, nil),
			{channelID: 1, modelID: "healthy"}: {},
		},
	}
	candidate := modelHealthCandidate(1, "failed", "healthy")
	candidate.modelAPIFormats = []string{"format-only"}

	orderCandidateModelsByHealth(context.Background(), provider, []*ChannelModelsCandidate{candidate})

	assert.Equal(t, []string{"failed", "healthy"}, []string{
		candidate.Models[0].ActualModel,
		candidate.Models[1].ActualModel,
	})
	assert.Equal(t, []string{"format-only"}, candidate.modelAPIFormats)
}

func TestErrorAwareStrategyUsesCandidateModelMetrics(t *testing.T) {
	channelFailure := modelHealthMetrics(3, nil)
	provider := &modelAwareMetricsProvider{
		channelMetrics: map[int]*biz.AggregatedMetrics{1: channelFailure},
		modelMetrics: map[modelHealthKey]*biz.AggregatedMetrics{
			{channelID: 1, modelID: "gpta"}: {},
		},
	}
	lb := NewLoadBalancer(provider, nil)
	strategy := NewErrorAwareStrategy()
	channel := &biz.Channel{Channel: &ent.Channel{ID: 1, Name: "test"}}

	// The composed snapshot replaces channel-level failures with the health
	// of the candidate's first model, so a healthy model scores full marks
	// even on a failing channel.
	composed, err := lb.candidateMetrics(contextWithCandidateModel(context.Background(), "gpta"), modelHealthCandidate(1, "gpta"))
	require.NoError(t, err)
	assert.Equal(t, 200.0, strategy.Score(context.Background(), channel, composed))

	// Without a candidate the channel-level failures apply.
	channelMetrics, err := lb.channelMetrics(context.Background(), channel)
	require.NoError(t, err)
	assert.Equal(t, 70.0, strategy.Score(context.Background(), channel, channelMetrics))
}

func TestRoundRobinHealthStrategyUsesHealthyModelWithinCandidate(t *testing.T) {
	failed := modelHealthMetrics(roundRobinFailureThreshold, nil)
	provider := &modelAwareMetricsProvider{
		channelMetrics: map[int]*biz.AggregatedMetrics{
			1: failed,
			2: failed,
			3: failed,
		},
		modelMetrics: map[modelHealthKey]*biz.AggregatedMetrics{
			{channelID: 1, modelID: "failed"}:   failed,
			{channelID: 1, modelID: "healthy"}:  {},
			{channelID: 2, modelID: "failed-a"}: failed,
			{channelID: 2, modelID: "failed-b"}: failed,
		},
	}
	lb := NewLoadBalancer(provider, nil)
	strategy := NewRoundRobinHealthStrategy()
	ctx := context.Background()

	// Health ordering runs before the load balancer, so the healthy model of a
	// mixed candidate is attempted first and the snapshot reflects it: the
	// candidate is not unhealthy.
	mixed := modelHealthCandidate(1, "failed", "healthy")
	orderCandidateModelsByHealth(ctx, provider, []*ChannelModelsCandidate{mixed})
	composed, err := lb.candidateMetrics(ctx, mixed)
	require.NoError(t, err)
	assert.False(t, strategy.IsUnhealthy(composed))

	// All models failing keeps the candidate unhealthy.
	allFailing := modelHealthCandidate(2, "failed-a", "failed-b")
	orderCandidateModelsByHealth(ctx, provider, []*ChannelModelsCandidate{allFailing})
	composed, err = lb.candidateMetrics(ctx, allFailing)
	require.NoError(t, err)
	assert.True(t, strategy.IsUnhealthy(composed))

	// A candidate without models falls back to channel-level health.
	bare := &ChannelModelsCandidate{
		Channel: &biz.Channel{Channel: &ent.Channel{ID: 3}},
	}
	composed, err = lb.candidateMetrics(ctx, bare)
	require.NoError(t, err)
	assert.True(t, strategy.IsUnhealthy(composed))
}

func TestModelHealthScenarioPrefersHealthyModelAndPenalizesFailedOnlyCandidate(t *testing.T) {
	provider := &modelAwareMetricsProvider{
		modelMetrics: map[modelHealthKey]*biz.AggregatedMetrics{
			{channelID: 1, modelID: "gptb"}: modelHealthMetrics(1, nil),
			{channelID: 1, modelID: "gpta"}: {},
			{channelID: 2, modelID: "gptc"}: modelHealthMetrics(1, nil),
		},
	}
	lb := NewLoadBalancer(provider, nil)
	strategy := NewErrorAwareStrategy()
	candidate := modelHealthCandidate(1, "gptb", "gpta")
	failedOnly := modelHealthCandidate(2, "gptc")
	ctx := context.Background()
	orderCandidateModelsByHealth(ctx, provider, []*ChannelModelsCandidate{candidate, failedOnly})

	assert.Equal(t, "gpta", healthModelOf(candidate))
	composedOne, err := lb.candidateMetrics(ctx, candidate)
	require.NoError(t, err)
	composedTwo, err := lb.candidateMetrics(ctx, failedOnly)
	require.NoError(t, err)
	channelOneScore := strategy.Score(ctx, candidate.Channel, composedOne)
	channelTwoScore := strategy.Score(ctx, failedOnly.Channel, composedTwo)

	assert.Equal(t, 200.0, channelOneScore)
	assert.Equal(t, 130.0, channelTwoScore)
}
