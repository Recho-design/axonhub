// 职责: 回归测试 Gemini 双向 ResponseFormat 的 null 泄漏，以及 snake_case schema 字段与大写类型递归降级。
// 关联文件:
//   llm/transformer/gemini/inbound_convert.go
//   llm/transformer/gemini/outbound_convert.go
//   llm/transformer/gemini/model.go
//   llm/internal/pkg/xjson/schema.go
// 遵循的 Rules:
//   .agents/rules/go-general.md

package gemini

import (
	"encoding/json"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
)

func TestConvertGeminiToLLMRequest_NullResponseJsonSchemaFallsBack(t *testing.T) {
	valid := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`)

	for _, nullSchema := range []json.RawMessage{
		json.RawMessage("null"),
		json.RawMessage("null "),
		json.RawMessage("{}"),
	} {
		t.Run(string(nullSchema), func(t *testing.T) {
			result, err := convertGeminiToLLMRequest(&GenerateContentRequest{
				Contents: []*Content{{Role: "user", Parts: []*Part{{Text: "hi"}}}},
				GenerationConfig: &GenerationConfig{
					ResponseJsonSchema: nullSchema,
					ResponseSchema:     valid,
					ResponseMIMEType:   "application/json",
				},
			})
			require.NoError(t, err)
			require.NotNil(t, result.ResponseFormat)
			require.Equal(t, "json_schema", result.ResponseFormat.Type)

			var wrapper struct {
				Schema json.RawMessage `json:"schema"`
			}
			require.NoError(t, json.Unmarshal(result.ResponseFormat.JSONSchema, &wrapper))
			require.JSONEq(t, string(valid), string(wrapper.Schema))
		})
	}
}

func TestConvertGeminiToLLMRequest_UnusableSchemasFallBackToJSONObject(t *testing.T) {
	result, err := convertGeminiToLLMRequest(&GenerateContentRequest{
		Contents: []*Content{{Role: "user", Parts: []*Part{{Text: "hi"}}}},
		GenerationConfig: &GenerationConfig{
			ResponseJsonSchema: json.RawMessage("null"),
			ResponseSchema:     json.RawMessage("null"),
			ResponseMIMEType:   "application/json",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, result.ResponseFormat)
	require.Equal(t, "json_object", result.ResponseFormat.Type)
	require.Nil(t, result.ResponseFormat.JSONSchema)
}

func TestExtractJSONSchema(t *testing.T) {
	require.Equal(t, "null", string(extractJSONSchema(json.RawMessage(`{"name":"x","schema":null}`))))
	require.Nil(t, extractJSONSchema(json.RawMessage(`{"name":"x"}`)))
	require.JSONEq(t, `{"type":"object","properties":{}}`,
		string(extractJSONSchema(json.RawMessage(`{"name":"x","schema":{"type":"object","properties":{}}}`))))
	require.JSONEq(t, `{"type":"string"}`,
		string(extractJSONSchema(json.RawMessage(`{"type":"string"}`))))
}

func TestConvertLLMToGeminiRequest_DropsUnusableResponseSchema(t *testing.T) {
	for _, wrapped := range []string{
		`{"name":"out"}`,
		`{"name":"out","schema":null}`,
		`{"name":"out","schema":{}}`,
		`{"name":"out","schema":{"type":"object"}}`,
	} {
		t.Run(wrapped, func(t *testing.T) {
			result := convertLLMToGeminiRequest(&llm.Request{
				Messages:       []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("hi")}}},
				ResponseFormat: &llm.ResponseFormat{Type: "json_schema", JSONSchema: json.RawMessage(wrapped)},
			})
			require.NotNil(t, result.GenerationConfig)
			require.Nil(t, result.GenerationConfig.ResponseJsonSchema,
				"Gemini rejects a null, empty, or property-less responseJsonSchema")
			require.Equal(t, "application/json", result.GenerationConfig.ResponseMIMEType)
		})
	}

	t.Run("usable schema is forwarded", func(t *testing.T) {
		schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`)
		result := convertLLMToGeminiRequest(&llm.Request{
			Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("hi")}}},
			ResponseFormat: &llm.ResponseFormat{
				Type:       "json_schema",
				JSONSchema: json.RawMessage(`{"name":"out","schema":` + string(schema) + `}`),
			},
		})
		require.JSONEq(t, string(schema), string(result.GenerationConfig.ResponseJsonSchema))
	})
}

func TestGenerationConfigUnmarshalSnakeCase(t *testing.T) {
	var gc GenerationConfig
	require.NoError(t, json.Unmarshal([]byte(`{
		"response_schema":{"type":"object","properties":{"a":{"type":"string"}}},
		"response_json_schema":{"type":"string"}
	}`), &gc))
	require.JSONEq(t, `{"type":"object","properties":{"a":{"type":"string"}}}`, string(gc.ResponseSchema))
	require.JSONEq(t, `{"type":"string"}`, string(gc.ResponseJsonSchema))
}

func TestFunctionDeclarationUnmarshalSnakeCase(t *testing.T) {
	var fd FunctionDeclaration
	require.NoError(t, json.Unmarshal([]byte(`{
		"name":"f",
		"parameters_json_schema":{"type":"object"},
		"response_json_schema":{"type":"string"}
	}`), &fd))
	require.Equal(t, "f", fd.Name)
	require.JSONEq(t, `{"type":"object"}`, string(fd.ParametersJsonSchema))
	require.JSONEq(t, `{"type":"string"}`, string(fd.ResponseJsonSchema))
}

// TestConvertGeminiToLLMRequest_LowercasesNestedSchemaTypes pins that uppercase
// types inside additionalProperties and type arrays are lowered recursively.
func TestConvertGeminiToLLMRequest_LowercasesNestedSchemaTypes(t *testing.T) {
	result, err := convertGeminiToLLMRequest(&GenerateContentRequest{
		Contents: []*Content{{Role: "user", Parts: []*Part{{Text: "hi"}}}},
		Tools: []*Tool{{FunctionDeclarations: []*FunctionDeclaration{{
			Name:                 "f",
			ParametersJsonSchema: json.RawMessage(`{"type":"OBJECT","additionalProperties":{"type":"STRING"}}`),
		}}}},
	})
	require.NoError(t, err)
	require.Len(t, result.Tools, 1)
	require.JSONEq(t,
		`{"type":"object","additionalProperties":{"type":"string"}}`,
		string(result.Tools[0].Function.ParametersJsonSchema))
}

func TestConvertGeminiToLLMRequest_LowercasesTypeArrays(t *testing.T) {
	result, err := convertGeminiToLLMRequest(&GenerateContentRequest{
		Contents: []*Content{{Role: "user", Parts: []*Part{{Text: "hi"}}}},
		Tools: []*Tool{{FunctionDeclarations: []*FunctionDeclaration{{
			Name:                 "f",
			ParametersJsonSchema: json.RawMessage(`{"type":"OBJECT","properties":{"x":{"type":["STRING","NULL"]}}}`),
		}}}},
	})
	require.NoError(t, err)
	require.JSONEq(t,
		`{"type":"object","properties":{"x":{"type":["string","null"]}}}`,
		string(result.Tools[0].Function.ParametersJsonSchema))
}
