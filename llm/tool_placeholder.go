// 职责: 为老旧反代提供可选的无参工具占位参数注入（统一请求与原始请求体两条链路）。
// 关联文件:
//
//	llm/tools.go
//	internal/server/orchestrator/transform_options.go
//	internal/server/orchestrator/pass_through.go
//	internal/server/orchestrator/override.go
//
// 遵循的 Rules:
//
//	.agents/rules/go-general.md
//
// 注意事项:
//
//	某些 OpenAI 兼容反代把 {"type":"object","properties":{}} 转成 Gemini 的
//	parameters: null 后自行返回 400，所以无参工具需要携带一个可选占位参数。
//	原始请求体注入只在确实需要改写时才重写字节，否则原样返回，保护上游 Prompt Cache。
//	占位参数在 strict 模式下必须进 required 且 additionalProperties:false。

package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/samber/lo"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ToolPlaceholderSchema is a no-argument function schema that still carries one
// optional property. Relays that rewrite an empty properties map into null keep
// a real record when this placeholder is present.
var ToolPlaceholderSchema = json.RawMessage(`{"type":"object","properties":{"_placeholder":{"type":"boolean","description":"Placeholder. Leave unset."}}}`)

// StrictToolPlaceholderSchema is ToolPlaceholderSchema made strict-mode
// compatible: OpenAI structured output requires every property in required and
// additionalProperties:false, so the placeholder is required and nullable.
var StrictToolPlaceholderSchema = json.RawMessage(`{"type":"object","properties":{"_placeholder":{"type":["boolean","null"],"description":"Placeholder. Always pass null."}},"required":["_placeholder"],"additionalProperties":false}`)

// ToolPlaceholderParameters returns a fresh copy of the placeholder schema,
// using the strict variant when the tool declares strict mode.
func ToolPlaceholderParameters(strict *bool) json.RawMessage {
	if strict != nil && *strict {
		return slices.Clone(StrictToolPlaceholderSchema)
	}

	return slices.Clone(ToolPlaceholderSchema)
}

// nonEmptySchemaConstraints are the keywords that turn an object schema into a
// real constraint even when it declares no properties.
var nonEmptySchemaConstraints = []string{
	"patternProperties", "anyOf", "oneOf", "allOf", "$ref", "not", "if", "enum", "const",
}

// IsEmptyParametersSchema reports whether a tool parameters schema declares no
// arguments at all: nil, blank, the literal null, the empty object literal, or
// an object whose type is absent or "object", whose properties are
// absent/null/empty, and which carries no other structural constraint.
// additionalProperties:false is tolerated because it constrains nothing when
// there are no properties.
func IsEmptyParametersSchema(schema json.RawMessage) bool {
	trimmed := bytes.TrimSpace(schema)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return true
	}

	if !gjson.ValidBytes(trimmed) {
		return false
	}

	root := gjson.ParseBytes(trimmed)
	if !root.IsObject() {
		return false
	}

	if typ := root.Get("type"); typ.Exists() && (typ.Type != gjson.String || typ.String() != "object") {
		return false
	}

	if props := root.Get("properties"); props.Exists() && props.Type != gjson.Null {
		if !props.IsObject() || len(props.Map()) != 0 {
			return false
		}
	}

	// A non-empty required list demands keys, so the schema is not a
	// no-argument declaration even without declared properties.
	if required := root.Get("required"); required.Exists() && required.Type != gjson.Null {
		if !required.IsArray() || len(required.Array()) != 0 {
			return false
		}
	}

	if ap := root.Get("additionalProperties"); ap.Exists() && ap.Type != gjson.False {
		return false
	}

	for _, key := range nonEmptySchemaConstraints {
		if root.Get(key).Exists() {
			return false
		}
	}

	return true
}

// InjectEmptyToolPlaceholders returns a copy of tools in which every
// no-argument function tool carries the placeholder parameters schema. The
// input slice and its Function values are never modified in place.
func InjectEmptyToolPlaceholders(tools []Tool) []Tool {
	if len(tools) == 0 {
		return tools
	}

	injected := make([]Tool, len(tools))
	copy(injected, tools)

	for i := range injected {
		if injected[i].Type != ToolTypeFunction {
			continue
		}

		if !IsEmptyParametersSchema(injected[i].Function.EffectiveParameters()) {
			continue
		}

		injected[i].Function.Parameters = ToolPlaceholderParameters(injected[i].Function.Strict)
		injected[i].Function.ParametersJsonSchema = nil
	}

	return injected
}

// InjectToolPlaceholders rewrites the tool declarations of a raw request body
// so that no-argument tools carry the placeholder parameters schema. The body
// is returned unchanged (same bytes) when nothing needs rewriting.
//
//nolint:gocyclo,cyclop // One branch per protocol is clearer than a table.
func InjectToolPlaceholders(body []byte, apiFormat APIFormat) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}

	//nolint:exhaustive // only formats carrying tool declarations need injection.
	switch apiFormat {
	case APIFormatOpenAIChatCompletion, APIFormatOllamaChat:
		return injectOpenAIChatToolPlaceholders(body)
	case APIFormatOpenAIResponse, APIFormatOpenAIResponseWebSocket, APIFormatOpenAIResponseCompact:
		return injectResponsesToolPlaceholders(body)
	case APIFormatAnthropicMessage:
		return injectAnthropicToolPlaceholders(body)
	case APIFormatGeminiContents:
		return injectGeminiToolPlaceholders(body)
	default:
		return body
	}
}

// rawJSONPatch replaces one raw JSON path, or deletes it when value is nil.
type rawJSONPatch struct {
	path  string
	value json.RawMessage
}

// applyRawJSONPatches applies patches one by one and keeps the original body
// when there is nothing to patch or a patch fails.
func applyRawJSONPatches(body []byte, patches []rawJSONPatch) []byte {
	if len(patches) == 0 {
		return body
	}

	out := body

	for _, patch := range patches {
		var err error

		if patch.value == nil {
			out, err = sjson.DeleteBytes(out, patch.path)
		} else {
			out, err = sjson.SetRawBytes(out, patch.path, patch.value)
		}

		if err != nil {
			return body
		}
	}

	return out
}

// isNonEmptySchemaResult reports whether a gjson result holds a usable schema.
func isNonEmptySchemaResult(result gjson.Result) bool {
	return result.Exists() && !IsEmptyParametersSchema(json.RawMessage(result.Raw))
}

// schemaStrict reads a "strict": true flag from a tool or function object.
func schemaStrict(result gjson.Result) *bool {
	strict := result.Get("strict")
	if !strict.Exists() || strict.Type != gjson.True {
		return nil
	}

	return lo.ToPtr(true)
}

// injectOpenAIChatToolPlaceholders patches tools[].function.parameters of an
// OpenAI Chat Completions or Ollama body. A missing parameters field counts as
// no-argument.
func injectOpenAIChatToolPlaceholders(body []byte) []byte {
	patches := make([]rawJSONPatch, 0)

	for i, tool := range gjson.GetBytes(body, "tools").Array() {
		// Only tools carrying a function object are patched; built-in tools
		// (web_search, image_generation, ...) have none.
		function := tool.Get("function")
		if !function.IsObject() {
			continue
		}

		if isNonEmptySchemaResult(function.Get("parameters")) {
			continue
		}

		patches = append(patches, rawJSONPatch{
			path:  fmt.Sprintf("tools.%d.function.parameters", i),
			value: ToolPlaceholderParameters(schemaStrict(function)),
		})
	}

	return applyRawJSONPatches(body, patches)
}

// injectResponsesToolPlaceholders patches tools[].parameters of a Responses
// body. A missing parameters field counts as no-argument, matching the unified
// path; the switch is opt-in, so the default omit-stays-omitted round trip is
// unaffected.
func injectResponsesToolPlaceholders(body []byte) []byte {
	patches := make([]rawJSONPatch, 0)

	for i, tool := range gjson.GetBytes(body, "tools").Array() {
		if !tool.IsObject() || tool.Get("type").String() != ToolTypeFunction {
			continue
		}

		if isNonEmptySchemaResult(tool.Get("parameters")) {
			continue
		}

		patches = append(patches, rawJSONPatch{
			path:  fmt.Sprintf("tools.%d.parameters", i),
			value: ToolPlaceholderParameters(schemaStrict(tool)),
		})
	}

	return applyRawJSONPatches(body, patches)
}

// injectAnthropicToolPlaceholders patches tools[].input_schema of an Anthropic
// body. Provider-native tools (web_search_*, ...) carry type values other than
// "custom" and are left alone.
func injectAnthropicToolPlaceholders(body []byte) []byte {
	patches := make([]rawJSONPatch, 0)

	for i, tool := range gjson.GetBytes(body, "tools").Array() {
		if !tool.IsObject() {
			continue
		}

		if toolType := tool.Get("type").String(); toolType != "" && toolType != "custom" {
			continue
		}

		if isNonEmptySchemaResult(tool.Get("input_schema")) {
			continue
		}

		patches = append(patches, rawJSONPatch{
			path:  fmt.Sprintf("tools.%d.input_schema", i),
			value: ToolPlaceholderParameters(schemaStrict(tool)),
		})
	}

	return applyRawJSONPatches(body, patches)
}

// injectGeminiToolPlaceholders patches function declarations of a Gemini
// contents body, accepting both the camelCase and snake_case declaration keys.
// The parameters and parametersJsonSchema fields are oneof: exactly one
// non-null key survives.
func injectGeminiToolPlaceholders(body []byte) []byte {
	patches := make([]rawJSONPatch, 0)

	for i, tool := range gjson.GetBytes(body, "tools").Array() {
		if !tool.IsObject() {
			continue
		}

		for _, declKey := range []string{"functionDeclarations", "function_declarations"} {
			declarations := tool.Get(declKey)
			if !declarations.IsArray() {
				continue
			}

			for j, declaration := range declarations.Array() {
				if !declaration.IsObject() {
					continue
				}

				declPath := fmt.Sprintf("tools.%d.%s.%d", i, declKey, j)
				patches = append(patches, geminiDeclarationPlaceholderPatches(declPath, declaration)...)
			}
		}
	}

	return applyRawJSONPatches(body, patches)
}

// geminiDeclarationPlaceholderPatches returns the patches that give one Gemini
// function declaration the placeholder schema. The parameters,
// parametersJsonSchema and parameters_json_schema fields are a oneof, so at
// most one non-null key may survive.
func geminiDeclarationPlaceholderPatches(declPath string, declaration gjson.Result) []rawJSONPatch {
	if geminiDeclarationHasParameters(declaration) {
		return nil
	}

	// Write into whichever key the caller already used; a fresh declaration
	// gets the camelCase field Gemini prefers.
	targetKey := "parametersJsonSchema"
	for _, key := range []string{"parameters", "parameters_json_schema"} {
		if declaration.Get(key).Exists() {
			targetKey = key
			break
		}
	}

	patches := []rawJSONPatch{{
		path:  declPath + "." + targetKey,
		value: ToolPlaceholderParameters(nil),
	}}

	// A leftover null sibling must not survive the oneof.
	for _, key := range []string{"parameters", "parametersJsonSchema", "parameters_json_schema"} {
		if key == targetKey {
			continue
		}

		if other := declaration.Get(key); other.Exists() && other.Type == gjson.Null {
			patches = append(patches, rawJSONPatch{path: declPath + "." + key})
		}
	}

	return patches
}

// geminiDeclarationHasParameters reports whether any of the Gemini oneof
// parameter keys holds a usable schema. The snake_case alias is included so a
// populated alias is never overwritten with a placeholder.
func geminiDeclarationHasParameters(declaration gjson.Result) bool {
	for _, key := range []string{"parameters", "parametersJsonSchema", "parameters_json_schema"} {
		if isNonEmptySchemaResult(declaration.Get(key)) {
			return true
		}
	}

	return false
}
