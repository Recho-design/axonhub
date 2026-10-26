package orchestrator

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
)

// TestDefaultSelector_ModelHealthReordersMultiModelCandidate exercises the real
// selection path for the user scenario: one aggregate model resolves to two
// upstream models in the same channel. The associations deliberately put gptb
// first so the post-failure assertion proves the health reorder rather than the
// input order. Regex associations produce the same candidate shape; their
// matching is covered by the association matcher tests.
func TestDefaultSelector_ModelHealthReordersMultiModelCandidate(t *testing.T) {
	ctx, client := setupTest(t)

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Multi Model Channel").
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		SetSupportedModels([]string{"gpta", "gptb"}).
		SetDefaultTestModel("gpta").
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	model, err := client.Model.Create().
		SetModelID("gpt6").
		SetName("GPT-6").
		SetDeveloper("openai").
		SetIcon("openai").
		SetGroup("gpt-6").
		SetModelCard(&objects.ModelCard{}).
		SetStatus("enabled").
		SetSettings(&objects.ModelSettings{
			Associations: []*objects.ModelAssociation{
				{
					Type:     "channel_model",
					Priority: 1,
					ChannelModel: &objects.ChannelModelAssociation{
						ChannelID: ch.ID,
						ModelID:   "gptb",
					},
				},
				{
					Type:     "channel_model",
					Priority: 1,
					ChannelModel: &objects.ChannelModelAssociation{
						ChannelID: ch.ID,
						ModelID:   "gpta",
					},
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	channelService := newTestChannelServiceForChannels(client)
	modelService := newTestModelService(client)
	systemService := newTestSystemService(client)
	selector := NewDefaultSelector(channelService, modelService, systemService)

	before, err := selector.Select(ctx, &llm.Request{Model: model.ModelID})
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.Len(t, before[0].Models, 2)
	require.Equal(t, "gptb", before[0].Models[0].ActualModel, "association order must put gptb first")
	require.Equal(t, "gpta", before[0].Models[1].ActualModel)

	now := time.Now()
	channelService.RecordPerformance(ctx, &biz.PerformanceRecord{
		ChannelID:          ch.ID,
		Model:              "gptb",
		StartTime:          now.Add(-time.Second),
		EndTime:            now,
		Success:            false,
		RequestCompleted:   true,
		ResponseStatusCode: 500,
	})

	after, err := selector.Select(ctx, &llm.Request{Model: model.ModelID})
	require.NoError(t, err)
	require.Len(t, after, 1)
	require.Len(t, after[0].Models, 2)
	require.Equal(t, "gpta", after[0].Models[0].ActualModel, "healthy model must be attempted first")
	require.Equal(t, "gptb", after[0].Models[1].ActualModel)
	require.Equal(t, "gpta", healthModelOf(after[0]), "candidate health must follow the healthy model")

	if len(after[0].modelAPIFormats) > 0 {
		require.Equal(t, after[0].modelAPIFormats[0], after[0].APIFormat,
			"candidate API format must follow the reordered model")
	}
}
