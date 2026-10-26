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
	strategy := NewErrorAwareStrategy(provider)
	channel := &biz.Channel{Channel: &ent.Channel{ID: 1, Name: "test"}}

	modelCtx := contextWithCandidateModel(context.Background(), "gpta")
	assert.Equal(t, 200.0, strategy.Score(modelCtx, channel))
	debugScore, details := strategy.ScoreWithDebug(modelCtx, channel)
	assert.Equal(t, 200.0, debugScore)
	assert.Equal(t, "gpta", details.Details["model"])
	assert.Equal(t, 70.0, strategy.Score(context.Background(), channel))
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
	strategy := NewRoundRobinHealthStrategy(provider)

	assert.False(t, strategy.IsUnhealthy(context.Background(), modelHealthCandidate(1, "failed", "healthy")))
	assert.True(t, strategy.IsUnhealthy(context.Background(), modelHealthCandidate(2, "failed-a", "failed-b")))
	assert.True(t, strategy.IsUnhealthy(context.Background(), &ChannelModelsCandidate{
		Channel: &biz.Channel{Channel: &ent.Channel{ID: 3}},
	}))
}

func TestModelHealthScenarioPrefersHealthyModelAndPenalizesFailedOnlyCandidate(t *testing.T) {
	provider := &modelAwareMetricsProvider{
		modelMetrics: map[modelHealthKey]*biz.AggregatedMetrics{
			{channelID: 1, modelID: "gptb"}: modelHealthMetrics(1, nil),
			{channelID: 1, modelID: "gpta"}: {},
			{channelID: 2, modelID: "gptc"}: modelHealthMetrics(1, nil),
		},
	}
	candidate := modelHealthCandidate(1, "gptb", "gpta")
	failedOnly := modelHealthCandidate(2, "gptc")
	orderCandidateModelsByHealth(context.Background(), provider, []*ChannelModelsCandidate{candidate, failedOnly})

	assert.Equal(t, "gpta", healthModelOf(candidate))
	strategy := NewErrorAwareStrategy(provider)
	channelOneScore := strategy.Score(contextWithCandidateModel(context.Background(), healthModelOf(candidate)), candidate.Channel)
	channelTwoScore := strategy.Score(contextWithCandidateModel(context.Background(), healthModelOf(failedOnly)), failedOnly.Channel)

	assert.Equal(t, 200.0, channelOneScore)
	assert.Equal(t, 130.0, channelTwoScore)
}
