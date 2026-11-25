package llm_test

import (
	"encoding/json"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
)

func TestContainsGoogleNativeTools(t *testing.T) {
	tests := []struct {
		name  string
		tools []llm.Tool
		want  bool
	}{
		{
			name:  "nil tools",
			tools: nil,
			want:  false,
		},
		{
			name:  "empty tools",
			tools: []llm.Tool{},
			want:  false,
		},
		{
			name: "only function tools",
			tools: []llm.Tool{
				{Type: "function", Function: llm.Function{Name: "get_weather"}},
				{Type: "function", Function: llm.Function{Name: "search"}},
			},
			want: false,
		},
		{
			name: "contains google_search",
			tools: []llm.Tool{
				{Type: "function", Function: llm.Function{Name: "get_weather"}},
				{Type: llm.ToolTypeGoogleSearch, Google: &llm.GoogleTools{Search: &llm.GoogleSearch{}}},
			},
			want: true,
		},
		{
			name: "contains google_url_context",
			tools: []llm.Tool{
				{Type: llm.ToolTypeGoogleUrlContext, Google: &llm.GoogleTools{UrlContext: &llm.GoogleUrlContext{}}},
			},
			want: true,
		},
		{
			name: "contains google_code_execution",
			tools: []llm.Tool{
				{Type: llm.ToolTypeGoogleCodeExecution, Google: &llm.GoogleTools{CodeExecution: &llm.GoogleCodeExecution{}}},
			},
			want: true,
		},
		{
			name: "contains multiple google native tools",
			tools: []llm.Tool{
				{Type: llm.ToolTypeGoogleSearch, Google: &llm.GoogleTools{Search: &llm.GoogleSearch{}}},
				{Type: llm.ToolTypeGoogleUrlContext, Google: &llm.GoogleTools{UrlContext: &llm.GoogleUrlContext{}}},
				{Type: "function", Function: llm.Function{Name: "get_weather"}},
			},
			want: true,
		},
		{
			name: "google native tool at the end",
			tools: []llm.Tool{
				{Type: "function", Function: llm.Function{Name: "fn1"}},
				{Type: "function", Function: llm.Function{Name: "fn2"}},
				{Type: llm.ToolTypeGoogleSearch, Google: &llm.GoogleTools{Search: &llm.GoogleSearch{}}},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := llm.ContainsGoogleNativeTools(tt.tools)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestIsGoogleNativeTool(t *testing.T) {
	tests := []struct {
		name string
		tool llm.Tool
		want bool
	}{
		{
			name: "function tool",
			tool: llm.Tool{Type: "function"},
			want: false,
		},
		{
			name: "image_generation tool",
			tool: llm.Tool{Type: llm.ToolTypeImageGeneration},
			want: false,
		},
		{
			name: "google_search tool",
			tool: llm.Tool{Type: llm.ToolTypeGoogleSearch},
			want: true,
		},
		{
			name: "google_url_context tool",
			tool: llm.Tool{Type: llm.ToolTypeGoogleUrlContext},
			want: true,
		},
		{
			name: "google_code_execution tool",
			tool: llm.Tool{Type: llm.ToolTypeGoogleCodeExecution},
			want: true,
		},
		{
			name: "empty type",
			tool: llm.Tool{Type: ""},
			want: false,
		},
		{
			name: "unknown type",
			tool: llm.Tool{Type: "unknown_tool_type"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := llm.IsGoogleNativeTool(tt.tool)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestFilterGoogleNativeTools(t *testing.T) {
	tests := []struct {
		name     string
		tools    []llm.Tool
		wantLen  int
		wantType []string
	}{
		{
			name:     "nil tools",
			tools:    nil,
			wantLen:  0,
			wantType: nil,
		},
		{
			name:     "empty tools",
			tools:    []llm.Tool{},
			wantLen:  0,
			wantType: nil,
		},
		{
			name: "only function tools - no filtering",
			tools: []llm.Tool{
				{Type: "function", Function: llm.Function{Name: "get_weather"}},
				{Type: "function", Function: llm.Function{Name: "search"}},
			},
			wantLen:  2,
			wantType: []string{"function", "function"},
		},
		{
			name: "filter google_search",
			tools: []llm.Tool{
				{Type: "function", Function: llm.Function{Name: "get_weather"}},
				{Type: llm.ToolTypeGoogleSearch, Google: &llm.GoogleTools{Search: &llm.GoogleSearch{}}},
			},
			wantLen:  1,
			wantType: []string{"function"},
		},
		{
			name: "filter all google native tools",
			tools: []llm.Tool{
				{Type: llm.ToolTypeGoogleSearch, Google: &llm.GoogleTools{Search: &llm.GoogleSearch{}}},
				{Type: "function", Function: llm.Function{Name: "get_weather"}},
				{Type: llm.ToolTypeGoogleUrlContext, Google: &llm.GoogleTools{UrlContext: &llm.GoogleUrlContext{}}},
				{Type: llm.ToolTypeGoogleCodeExecution, Google: &llm.GoogleTools{CodeExecution: &llm.GoogleCodeExecution{}}},
			},
			wantLen:  1,
			wantType: []string{"function"},
		},
		{
			name: "all google native tools - empty result",
			tools: []llm.Tool{
				{Type: llm.ToolTypeGoogleSearch, Google: &llm.GoogleTools{Search: &llm.GoogleSearch{}}},
				{Type: llm.ToolTypeGoogleUrlContext, Google: &llm.GoogleTools{UrlContext: &llm.GoogleUrlContext{}}},
			},
			wantLen:  0,
			wantType: []string{},
		},
		{
			name: "mixed tools with multiple function tools",
			tools: []llm.Tool{
				{Type: "function", Function: llm.Function{Name: "fn1"}},
				{Type: llm.ToolTypeGoogleSearch, Google: &llm.GoogleTools{Search: &llm.GoogleSearch{}}},
				{Type: "function", Function: llm.Function{Name: "fn2"}},
				{Type: llm.ToolTypeGoogleCodeExecution, Google: &llm.GoogleTools{CodeExecution: &llm.GoogleCodeExecution{}}},
				{Type: "function", Function: llm.Function{Name: "fn3"}},
			},
			wantLen:  3,
			wantType: []string{"function", "function", "function"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := llm.FilterGoogleNativeTools(tt.tools)
			require.Len(t, got, tt.wantLen)

			if len(tt.wantType) > 0 {
				gotTypes := make([]string, len(got))
				for i, tool := range got {
					gotTypes[i] = tool.Type
				}

				require.Equal(t, tt.wantType, gotTypes)
			}
		})
	}
}

func TestNormalizeEmptySchema(t *testing.T) {
	tests := []struct {
		name   string
		schema json.RawMessage
		want   json.RawMessage
	}{
		{
			name:   "nil schema becomes empty object schema",
			schema: nil,
			want:   llm.EmptyObjectSchema,
		},
		{
			name:   "blank schema becomes empty object schema",
			schema: json.RawMessage("  "),
			want:   llm.EmptyObjectSchema,
		},
		{
			name:   "literal null becomes empty object schema",
			schema: json.RawMessage("null"),
			want:   llm.EmptyObjectSchema,
		},
		{
			name:   "padded null becomes empty object schema",
			schema: json.RawMessage(" null "),
			want:   llm.EmptyObjectSchema,
		},
		{
			name:   "valid schema passes through unchanged",
			schema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
			want:   json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.JSONEq(t, string(tt.want), string(llm.NormalizeEmptySchema(tt.schema)))
		})
	}
}

func TestNormalizeFunctionParameters(t *testing.T) {
	validSchema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)

	tests := []struct {
		name   string
		schema json.RawMessage
		strict *bool
		want   json.RawMessage
	}{
		{
			name:   "nil strict with null schema uses plain empty object schema",
			schema: json.RawMessage("null"),
			strict: nil,
			want:   llm.EmptyObjectSchema,
		},
		{
			name:   "strict false with null schema uses plain empty object schema",
			schema: json.RawMessage("null"),
			strict: lo.ToPtr(false),
			want:   llm.EmptyObjectSchema,
		},
		{
			name:   "strict true with null schema satisfies strict validation",
			schema: json.RawMessage("null"),
			strict: lo.ToPtr(true),
			want:   llm.StrictEmptyObjectSchema,
		},
		{
			name:   "strict true with valid schema passes through unchanged",
			schema: validSchema,
			strict: lo.ToPtr(true),
			want:   validSchema,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.JSONEq(t, string(tt.want), string(llm.NormalizeFunctionParameters(tt.schema, tt.strict)))
		})
	}
}

func TestFunctionEffectiveParameters(t *testing.T) {
	valid := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	otherValid := json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"}}}`)

	tests := []struct {
		name string
		item llm.Function
		want json.RawMessage
	}{
		{
			name: "both unset yields nil",
			item: llm.Function{},
			want: nil,
		},
		{
			name: "legacy parameters only",
			item: llm.Function{Parameters: valid},
			want: valid,
		},
		{
			name: "json schema only",
			item: llm.Function{ParametersJsonSchema: valid},
			want: valid,
		},
		{
			name: "non-empty json schema wins over parameters",
			item: llm.Function{Parameters: otherValid, ParametersJsonSchema: valid},
			want: valid,
		},
		{
			name: "empty json schema defers to usable parameters",
			item: llm.Function{ParametersJsonSchema: json.RawMessage("{}"), Parameters: valid},
			want: valid,
		},
		{
			name: "empty json schema with null parameters stays empty object",
			item: llm.Function{ParametersJsonSchema: json.RawMessage("{}"), Parameters: json.RawMessage("null")},
			want: json.RawMessage("{}"),
		},
		{
			name: "null json schema defers to usable parameters",
			item: llm.Function{ParametersJsonSchema: json.RawMessage("null"), Parameters: valid},
			want: valid,
		},
		{
			name: "explicit null json schema is preserved when parameters unset",
			item: llm.Function{ParametersJsonSchema: json.RawMessage("null")},
			want: json.RawMessage("null"),
		},
		{
			name: "null parameters alone are returned as-is",
			item: llm.Function{Parameters: json.RawMessage("null")},
			want: json.RawMessage("null"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.item.EffectiveParameters()
			require.Equal(t, []byte(tt.want), []byte(got))
		})
	}
}

func TestNormalizeFunctionParameters_PreservesComplexSchemas(t *testing.T) {
	// These schemas carry real constraints. Rewriting them would change the
	// contract the model sees, so they must pass through byte-for-byte.
	inputs := []json.RawMessage{
		json.RawMessage(`{"type":"object","additionalProperties":{"type":"string"}}`),
		json.RawMessage(`{"type":"object","anyOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}`),
		json.RawMessage(`{"type":"object","oneOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}`),
		json.RawMessage(`{"type":"object","allOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}`),
		json.RawMessage(`{"type":"object","patternProperties":{"^x-":{"type":"string"}}}`),
		json.RawMessage(`{"type":"object","required":["x"],"properties":{"x":{"type":"string"}}}`),
		json.RawMessage(`{"type":"object","additionalProperties":true}`),
		json.RawMessage(`{"type":"string"}`),
		json.RawMessage(`{"type":["object","null"]}`),
		json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`),
	}

	for _, input := range inputs {
		t.Run(string(input), func(t *testing.T) {
			for _, strict := range []*bool{nil, lo.ToPtr(false), lo.ToPtr(true)} {
				got := llm.NormalizeFunctionParameters(input, strict)
				require.Equal(t, []byte(input), []byte(got), "schema must not be rewritten")
			}
			require.Equal(t, []byte(input), []byte(llm.NormalizeEmptySchema(input)))
		})
	}
}

func TestNormalizeEmptySchema_ReturnsIndependentClones(t *testing.T) {
	plain := llm.NormalizeEmptySchema(nil)
	require.JSONEq(t, string(llm.EmptyObjectSchema), string(plain))

	plain[0] = 'X'
	require.JSONEq(t, `{"type":"object","properties":{}}`, string(llm.EmptyObjectSchema),
		"mutating the returned schema must not corrupt the shared template")

	strict := llm.NormalizeFunctionParameters(nil, lo.ToPtr(true))
	require.JSONEq(t, string(llm.StrictEmptyObjectSchema), string(strict))

	strict[0] = 'X'
	require.JSONEq(t, `{"type":"object","properties":{},"required":[],"additionalProperties":false}`,
		string(llm.StrictEmptyObjectSchema),
		"mutating the returned strict schema must not corrupt the shared template")

	again := llm.NormalizeEmptySchema(json.RawMessage("{}"))
	require.JSONEq(t, `{"type":"object","properties":{}}`, string(llm.EmptyObjectSchema))
	require.JSONEq(t, `{"type":"object","properties":{}}`, string(again))
}
