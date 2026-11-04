package objects

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChannelModelAssociationUnmarshalJSONLegacyModelID(t *testing.T) {
	var assoc ChannelModelAssociation
	require.NoError(t, json.Unmarshal([]byte(`{"channelId":1,"modelId":"gpt-4"}`), &assoc))

	require.Equal(t, 1, assoc.ChannelID)
	require.Equal(t, []string{"gpt-4"}, assoc.ModelIDs)
}

func TestChannelModelAssociationUnmarshalJSONModelIDs(t *testing.T) {
	var assoc ChannelModelAssociation
	require.NoError(t, json.Unmarshal([]byte(`{"channelId":1,"modelIds":["gpt-4","gpt-4o"]}`), &assoc))

	require.Equal(t, 1, assoc.ChannelID)
	require.Equal(t, []string{"gpt-4", "gpt-4o"}, assoc.ModelIDs)
}

func TestChannelModelAssociationUnmarshalJSONPrefersModelIDs(t *testing.T) {
	var assoc ChannelModelAssociation
	require.NoError(t, json.Unmarshal([]byte(`{"channelId":1,"modelId":"legacy","modelIds":["gpt-4o"]}`), &assoc))

	require.Equal(t, []string{"gpt-4o"}, assoc.ModelIDs)
}

func TestModelSettingsUnmarshalJSONLegacyChannelModelModelID(t *testing.T) {
	var settings ModelSettings
	require.NoError(t, json.Unmarshal([]byte(`{
		"associations":[{"type":"channel_model","channelModel":{"channelId":7,"modelId":"gpt-4"}}]
	}`), &settings))

	require.Len(t, settings.Associations, 1)
	require.Equal(t, []string{"gpt-4"}, settings.Associations[0].ChannelModel.ModelIDs)
}
