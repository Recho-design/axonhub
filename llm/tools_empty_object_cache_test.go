// 职责: 回归测试空对象 Schema 归一化对合法字节序、additionalProperties 约束与 required:null 的处理。
// 关联文件:
//   llm/tools.go
// 遵循的 Rules:
//   .agents/rules/go-general.md

package llm_test

import (
	"encoding/json"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
)

// TestNormalizeFunctionParameters_PreservesCompleteEmptyObjectBytes pins that an
// already-valid no-argument schema keeps its exact bytes (key order/whitespace)
// and never loses an explicit additionalProperties:false.
func TestNormalizeFunctionParameters_PreservesCompleteEmptyObjectBytes(t *testing.T) {
	inputs := []json.RawMessage{
		json.RawMessage(`{ "properties": {}, "type": "object" }`),
		json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
		json.RawMessage(`{"type":"object","properties":{},"description":"no args"}`),
	}

	for _, input := range inputs {
		t.Run(string(input), func(t *testing.T) {
			require.Equal(t, []byte(input), []byte(llm.NormalizeFunctionParameters(input, nil)))
			require.Equal(t, []byte(input), []byte(llm.NormalizeFunctionParameters(input, lo.ToPtr(false))))
		})
	}
}

// TestNormalizeFunctionParameters_StrictCompleteEmptyObjectBytes pins that a
// strict-mode schema that already declares required:[] and
// additionalProperties:false is not rewritten.
func TestNormalizeFunctionParameters_StrictCompleteEmptyObjectBytes(t *testing.T) {
	input := json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`)
	require.Equal(t, []byte(input), []byte(llm.NormalizeFunctionParameters(input, lo.ToPtr(true))))
}

// TestNormalizeFunctionParameters_CleansRequiredNull pins that a stale
// required:null is removed instead of forwarded.
func TestNormalizeFunctionParameters_CleansRequiredNull(t *testing.T) {
	got := llm.NormalizeFunctionParameters(
		json.RawMessage(`{"type":"object","properties":{},"required":null}`), nil)
	require.JSONEq(t, `{"type":"object","properties":{}}`, string(got))
	require.NotContains(t, string(got), `"required"`)

	strict := llm.NormalizeFunctionParameters(
		json.RawMessage(`{"type":"object","required":null}`), lo.ToPtr(true))
	require.JSONEq(t, string(llm.StrictEmptyObjectSchema), string(strict))
}

// TestNormalizeFunctionParameters_PreservesAdditionalPropertiesOnPropertylessShell
// pins that a shell without properties keeps additionalProperties:false while
// gaining the empty properties map.
func TestNormalizeFunctionParameters_PreservesAdditionalPropertiesOnPropertylessShell(t *testing.T) {
	got := llm.NormalizeFunctionParameters(
		json.RawMessage(`{"type":"object","additionalProperties":false}`), nil)
	require.JSONEq(t, `{"type":"object","properties":{},"additionalProperties":false}`, string(got))
}

// TestIsEmptyObjectSchema covers the exported helper used to reject schemas
// that Gemini/Antigravity refuse for response schemas.
func TestIsEmptyObjectSchema(t *testing.T) {
	empty := []json.RawMessage{
		nil,
		json.RawMessage(``),
		json.RawMessage(`null`),
		json.RawMessage(`{}`),
		json.RawMessage(`{"type":"object"}`),
		json.RawMessage(`{"type":"object","properties":{}}`),
		json.RawMessage(`{"type":"object","description":"no args"}`),
	}
	for _, raw := range empty {
		require.True(t, llm.IsEmptyObjectSchema(raw), "expected empty schema: %s", string(raw))
	}

	nonEmpty := []json.RawMessage{
		json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
		json.RawMessage(`{"type":"object","additionalProperties":{"type":"string"}}`),
		json.RawMessage(`{"type":"object","anyOf":[{"type":"object"}]}`),
		json.RawMessage(`{"type":"string"}`),
	}
	for _, raw := range nonEmpty {
		require.False(t, llm.IsEmptyObjectSchema(raw), "expected non-empty schema: %s", string(raw))
	}
}
