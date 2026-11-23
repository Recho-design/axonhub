// 职责: 回归测试 Responses 命名空间工具原始回放时的 parameters:null 清洗，以及文本输出格式与函数参数的 null/复杂 Schema 处理。
// 关联文件:
//   llm/transformer/openai/responses/request_extensions.go
//   llm/transformer/openai/responses/outbound_convert.go
// 遵循的 Rules:
//   .agents/rules/go-general.md

package responses

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestSanitizeRawToolFragment_NullParameters(t *testing.T) {
	t.Run("strict null parameters become the strict empty object schema", func(t *testing.T) {
		raw := json.RawMessage(`{"type":"namespace","name":"docs","tools":[{"type":"function","name":"ping","strict":true,"parameters":null}]}`)
		got := sanitizeRawToolFragment(raw)
		require.JSONEq(t, `{"type":"namespace","name":"docs","tools":[{"type":"function","name":"ping","strict":true,"parameters":{"type":"object","properties":{},"required":[],"additionalProperties":false}}]}`, string(got))
	})

	t.Run("non-strict null parameters become the plain empty object schema", func(t *testing.T) {
		raw := json.RawMessage(`{"type":"namespace","name":"docs","tools":[{"type":"function","name":"ping","parameters":null}]}`)
		got := sanitizeRawToolFragment(raw)
		require.JSONEq(t, `{"type":"namespace","name":"docs","tools":[{"type":"function","name":"ping","parameters":{"type":"object","properties":{}}}]}`, string(got))
	})

	t.Run("omitted parameters stay byte-identical", func(t *testing.T) {
		raw := json.RawMessage(`{"type":"namespace","name":"docs","tools":[{"type":"function","name":"ping"}]}`)
		require.Equal(t, string(raw), string(sanitizeRawToolFragment(raw)))
	})

	t.Run("non-namespace fragments are untouched", func(t *testing.T) {
		raw := json.RawMessage(`{"type":"function","name":"ping","parameters":null}`)
		require.Equal(t, string(raw), string(sanitizeRawToolFragment(raw)))
	})
}

// TestNamespaceReview_NullParametersDoNotReplayUpstream exercises the full
// inbound -> outbound path: a namespace sub-tool declaring parameters:null must
// not be replayed verbatim to the provider.
func TestNamespaceReview_NullParametersDoNotReplayUpstream(t *testing.T) {
	data := []byte(`{"model":"test","input":"hi","tools":[{"type":"namespace","name":"docs","tools":[{"type":"function","name":"ping","strict":true,"parameters":null}]}]}`)
	req, err := NewInboundTransformer().TransformRequest(t.Context(), &httpclient.Request{Body: data})
	require.NoError(t, err)

	// The namespace must be preserved as a raw replay fragment for this test to
	// exercise the raw path; its stored copy must already be sanitized.
	require.NotNil(t, req.ProviderExtensions)
	require.NotNil(t, req.ProviderExtensions.OpenAIResponses)
	require.NotNil(t, req.ProviderExtensions.OpenAIResponses.Request)
	require.Len(t, req.ProviderExtensions.OpenAIResponses.Request.RawTools, 1)
	require.NotContains(t,
		string(req.ProviderExtensions.OpenAIResponses.Request.RawTools[0].Raw), `"parameters":null`)

	out, err := NewOutboundTransformer("https://example.com", "test")
	require.NoError(t, err)
	wire, err := out.TransformRequest(t.Context(), req)
	require.NoError(t, err)

	require.NotContains(t, string(wire.Body), `"parameters":null`)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(wire.Body, &payload))
	tools, ok := payload["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)

	namespace, ok := tools[0].(map[string]any)
	require.True(t, ok)
	subTools, ok := namespace["tools"].([]any)
	require.True(t, ok)
	require.Len(t, subTools, 1)

	sub, ok := subTools[0].(map[string]any)
	require.True(t, ok)
	params, ok := sub["parameters"].(map[string]any)
	require.True(t, ok, "parameters must be an object, not null")
	require.Equal(t, "object", params["type"])
}

func TestConvertToTextOptions_NullSchemaIsOmitted(t *testing.T) {
	for _, wrapped := range []string{
		`{"name":"out","schema":null}`,
		`{"name":"out"}`,
	} {
		t.Run(wrapped, func(t *testing.T) {
			chatReq := &llm.Request{
				ResponseFormat: &llm.ResponseFormat{
					Type:       "json_schema",
					JSONSchema: json.RawMessage(wrapped),
				},
			}
			opts := convertToTextOptions(chatReq)
			require.NotNil(t, opts)
			require.NotNil(t, opts.Format)
			require.Nil(t, opts.Format.Schema)

			body, err := json.Marshal(opts)
			require.NoError(t, err)
			require.NotContains(t, string(body), `"schema":null`)
		})
	}

	t.Run("usable schema is forwarded", func(t *testing.T) {
		chatReq := &llm.Request{
			ResponseFormat: &llm.ResponseFormat{
				Type:       "json_schema",
				JSONSchema: json.RawMessage(`{"name":"out","schema":{"type":"object","properties":{"a":{"type":"string"}}}}`),
			},
		}
		opts := convertToTextOptions(chatReq)
		require.NotNil(t, opts.Format)
		require.JSONEq(t, `{"type":"object","properties":{"a":{"type":"string"}}}`, string(opts.Format.Schema))
	})
}

// TestConvertFunctionToTool_DoesNotForcePropertiesOnComplexObject pins that a
// dynamic dictionary or a top-level union is never padded with properties:{}.
func TestConvertFunctionToTool_DoesNotForcePropertiesOnComplexObject(t *testing.T) {
	for _, params := range []json.RawMessage{
		json.RawMessage(`{"type":"object","additionalProperties":{"type":"string"}}`),
		json.RawMessage(`{"type":"object","anyOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}`),
		json.RawMessage(`{"type":"object","oneOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}`),
		json.RawMessage(`{"type":"object","allOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}`),
	} {
		t.Run(string(params), func(t *testing.T) {
			tool := convertFunctionToTool(llm.Tool{
				Type:     "function",
				Function: llm.Function{Name: "f", Parameters: params},
			})
			require.NotNil(t, tool.Parameters)
			require.NotContains(t, tool.Parameters, "properties",
				"a constraint-bearing object schema must not gain properties:{}")
		})
	}
}
