// 职责: 回归测试 Antigravity 请求信封禁用原始 Body 直通，以及 ResponseSchema/Parameters 的 null 与非法 Schema 兜底。
// 关联文件:
//   llm/transformer/antigravity/transformer.go
//   llm/transformer/antigravity/sanitizer.go
// 遵循的 Rules:
//   .agents/rules/go-general.md

package antigravity

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/gemini"
)

func TestTransformer_AllowPassThroughBody(t *testing.T) {
	require.False(t, (&Transformer{}).AllowPassThroughBody(
		context.Background(), &llm.Request{}, &httpclient.Request{}),
		"Antigravity wraps the Gemini body in an envelope and must never reuse the raw Gemini body")
}

func TestPatchGeminiRequest_NullResponseJsonSchemaFallsBack(t *testing.T) {
	for _, unusable := range []json.RawMessage{
		json.RawMessage("null"),
		json.RawMessage(" {} "),
		json.RawMessage(`{"type":"object"}`),
	} {
		t.Run(string(unusable), func(t *testing.T) {
			req := &gemini.GenerateContentRequest{
				GenerationConfig: &gemini.GenerationConfig{
					ResponseJsonSchema: unusable,
					ResponseSchema:     json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`),
				},
			}
			require.NoError(t, (&Transformer{}).patchGeminiRequest(context.Background(), req, &llm.Request{}))

			require.Nil(t, req.GenerationConfig.ResponseJsonSchema)
			require.NotNil(t, req.GenerationConfig.ResponseSchema)
			require.Contains(t, string(req.GenerationConfig.ResponseSchema), `"OBJECT"`)
			require.Contains(t, string(req.GenerationConfig.ResponseSchema), `"STRING"`)
		})
	}
}

func TestPatchGeminiRequest_UnusableResponseSchemasAreCleared(t *testing.T) {
	for _, schema := range []json.RawMessage{
		json.RawMessage("null"),
		json.RawMessage(`{"type":"object"}`),
		json.RawMessage(`not json`),
	} {
		t.Run(string(schema), func(t *testing.T) {
			req := &gemini.GenerateContentRequest{
				GenerationConfig: &gemini.GenerationConfig{
					ResponseJsonSchema: schema,
					ResponseSchema:     json.RawMessage("null"),
				},
			}
			require.NoError(t, (&Transformer{}).patchGeminiRequest(context.Background(), req, &llm.Request{}))
			require.Nil(t, req.GenerationConfig.ResponseSchema)
			require.Nil(t, req.GenerationConfig.ResponseJsonSchema)
		})
	}
}

func TestPatchGeminiRequest_InvalidParametersJsonSchemaFallsBack(t *testing.T) {
	t.Run("non-object parametersJsonSchema defers to usable parameters", func(t *testing.T) {
		req := &gemini.GenerateContentRequest{
			Tools: []*gemini.Tool{{FunctionDeclarations: []*gemini.FunctionDeclaration{{
				Name:                 "f",
				ParametersJsonSchema: json.RawMessage(`false`),
				Parameters:           json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
			}}}},
		}
		require.NoError(t, (&Transformer{}).patchGeminiRequest(context.Background(), req, &llm.Request{}))

		fd := req.Tools[0].FunctionDeclarations[0]
		require.Nil(t, fd.ParametersJsonSchema)
		require.NotNil(t, fd.Parameters)
		require.NotContains(t, string(fd.Parameters), `"null"`)
		require.Contains(t, string(fd.Parameters), `"STRING"`)
	})

	t.Run("null parameters never survive", func(t *testing.T) {
		for _, invalid := range []json.RawMessage{
			json.RawMessage(`[1]`),
			json.RawMessage(`false`),
			json.RawMessage(`"nope"`),
		} {
			req := &gemini.GenerateContentRequest{
				Tools: []*gemini.Tool{{FunctionDeclarations: []*gemini.FunctionDeclaration{{
					Name:                 "f",
					ParametersJsonSchema: invalid,
					Parameters:           json.RawMessage("null"),
				}}}},
			}
			require.NoError(t, (&Transformer{}).patchGeminiRequest(context.Background(), req, &llm.Request{}))

			fd := req.Tools[0].FunctionDeclarations[0]
			require.Nil(t, fd.ParametersJsonSchema)
			require.NotNil(t, fd.Parameters)
			require.NotContains(t, string(fd.Parameters), `"null"`)

			var params map[string]any
			require.NoError(t, json.Unmarshal(fd.Parameters, &params))
			require.Equal(t, "OBJECT", params["type"])
		}
	})
}
