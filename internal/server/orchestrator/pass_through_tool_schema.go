// 职责: 修补直通、Responses 传输适配与渠道覆写请求体中被显式写成 null 的工具参数 schema。
// 关联文件:
//
//	internal/server/orchestrator/pass_through.go
//	internal/server/orchestrator/override.go
//	llm/tools.go
//
// 遵循的 Rules:
//
//	.agents/rules/go-general.md
//
// 注意事项:
//
//	只有目标字段存在且值为 JSON null 时才改写字节流，其余情况必须原样返回，避免破坏上游 Prompt Cache。
//	Gemini 的 parameters、parametersJsonSchema 与 parameters_json_schema 是 oneof 互斥关系，修补后只能保留唯一一个非 null 字段。
//	Antigravity 把整个请求包在顶层 request 信封里，Gemini 修补必须同时覆盖 request.tools 与 request.generationConfig。
package orchestrator

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/llm"
)

// sanitizeNullToolSchemasInRawBody replaces an explicitly null tool parameter schema
// with an empty object schema so a replayed or overridden body is accepted upstream.
// The body bytes are never touched when no target path holds a JSON null.
func sanitizeNullToolSchemasInRawBody(body []byte, apiFormat llm.APIFormat) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}

	//nolint:exhaustive // only formats carrying tool declarations need patching.
	switch apiFormat {
	case llm.APIFormatOpenAIChatCompletion, llm.APIFormatOllamaChat:
		return sanitizeOpenAIChatToolSchemas(body)
	case llm.APIFormatOpenAIResponse, llm.APIFormatOpenAIResponseWebSocket:
		return sanitizeOpenAIResponsesToolSchemas(body)
	case llm.APIFormatAnthropicMessage:
		return sanitizeAnthropicToolSchemas(body)
	case llm.APIFormatGeminiContents:
		return sanitizeGeminiToolSchemas(body)
	default:
		return body
	}
}

// nullToolSchemaAt reports whether path exists and currently holds an explicit JSON null.
func nullToolSchemaAt(body []byte, path string) bool {
	result := gjson.GetBytes(body, path)

	return result.Exists() && result.Type == gjson.Null
}

// replaceNullToolSchema rewrites path to an empty object schema only when it currently
// holds an explicit JSON null. A missing or already-populated schema is left untouched.
func replaceNullToolSchema(body []byte, path string, strict bool) []byte {
	if !nullToolSchemaAt(body, path) {
		return body
	}

	schema := llm.EmptyObjectSchema
	if strict {
		schema = llm.StrictEmptyObjectSchema
	}

	updated, err := sjson.SetRawBytes(body, path, schema)
	if err != nil {
		return body
	}

	return updated
}

// deleteNullToolSchema removes path only when it currently holds an explicit JSON null.
func deleteNullToolSchema(body []byte, path string) []byte {
	if !nullToolSchemaAt(body, path) {
		return body
	}

	updated, err := sjson.DeleteBytes(body, path)
	if err != nil {
		return body
	}

	return updated
}

// sanitizeOpenAIChatToolSchemas patches tools[].function.parameters and the legacy
// top-level functions[].parameters of an OpenAI Chat Completions body.
func sanitizeOpenAIChatToolSchemas(body []byte) []byte {
	for i, tool := range gjson.GetBytes(body, "tools").Array() {
		if !tool.IsObject() {
			continue
		}

		prefix := fmt.Sprintf("tools.%d.function", i)
		body = replaceNullToolSchema(body, prefix+".parameters", gjson.GetBytes(body, prefix+".strict").Bool())
	}

	for i, function := range gjson.GetBytes(body, "functions").Array() {
		if !function.IsObject() {
			continue
		}

		body = replaceNullToolSchema(body, fmt.Sprintf("functions.%d.parameters", i), false)
	}

	return body
}

// sanitizeOpenAIResponsesToolSchemas patches tools[].parameters and the nested
// tools[].tools[].parameters of OpenAI Responses bodies.
func sanitizeOpenAIResponsesToolSchemas(body []byte) []byte {
	for i, tool := range gjson.GetBytes(body, "tools").Array() {
		if !tool.IsObject() {
			continue
		}

		toolPath := fmt.Sprintf("tools.%d", i)
		if tool.Get("type").String() == "namespace" {
			body = sanitizeOpenAIResponsesNamespaceTools(body, toolPath)
			continue
		}

		if !isResponsesFunctionTool(body, toolPath) {
			continue
		}

		body = replaceNullToolSchema(body, toolPath+".parameters", gjson.GetBytes(body, toolPath+".strict").Bool())
	}

	return body
}

// sanitizeOpenAIResponsesNamespaceTools patches the function tools nested inside a
// Responses namespace tool.
func sanitizeOpenAIResponsesNamespaceTools(body []byte, namespacePath string) []byte {
	namespaceToolsPath := namespacePath + ".tools"

	for j, tool := range gjson.GetBytes(body, namespaceToolsPath).Array() {
		if !tool.IsObject() {
			continue
		}

		toolPath := fmt.Sprintf("%s.%d", namespaceToolsPath, j)
		if !isResponsesFunctionTool(body, toolPath) {
			continue
		}

		body = replaceNullToolSchema(body, toolPath+".parameters", gjson.GetBytes(body, toolPath+".strict").Bool())
	}

	return body
}

// isResponsesFunctionTool reports whether a Responses tool entry declares parameters:
// either an explicit "function" type or a bare tool carrying a parameters field.
func isResponsesFunctionTool(body []byte, toolPath string) bool {
	switch gjson.GetBytes(body, toolPath+".type").String() {
	case "function":
		return true
	case "":
		return gjson.GetBytes(body, toolPath+".parameters").Exists()
	default:
		return false
	}
}

// sanitizeAnthropicToolSchemas patches tools[].input_schema of an Anthropic body.
func sanitizeAnthropicToolSchemas(body []byte) []byte {
	for i, tool := range gjson.GetBytes(body, "tools").Array() {
		if !tool.IsObject() {
			continue
		}

		toolPath := fmt.Sprintf("tools.%d", i)
		body = replaceNullToolSchema(body, toolPath+".input_schema", gjson.GetBytes(body, toolPath+".strict").Bool())
	}

	return body
}

// geminiParameterSchemaKeys are the candidate keys carrying a Gemini function's input
// schema. Gemini treats them as a oneof, so at most one may survive sanitizing.
var geminiParameterSchemaKeys = []string{"parameters", "parametersJsonSchema", "parameters_json_schema"}

// geminiResponseSchemaKeys are the optional function output schema keys. Gemini rejects
// an explicit null there, so such a value is deleted instead of being replaced.
var geminiResponseSchemaKeys = []string{"response", "responseJsonSchema", "response_json_schema"}

// geminiGenerationResponseSchemaKeys are the response schema keys nested under a Gemini
// generationConfig block, which equally reject an explicit null.
var geminiGenerationResponseSchemaKeys = []string{
	"responseSchema", "response_schema", "responseJsonSchema", "response_json_schema",
}

// sanitizeGeminiToolSchemas patches function declarations of a Gemini contents body,
// accepting both the camelCase and snake_case declaration keys. Antigravity nests the
// whole payload in a top-level "request" envelope, so that layout is patched as well.
func sanitizeGeminiToolSchemas(body []byte) []byte {
	body = sanitizeGeminiBodyAtPrefix(body, "")
	if gjson.GetBytes(body, "request").IsObject() {
		body = sanitizeGeminiBodyAtPrefix(body, "request.")
	}

	return body
}

// sanitizeGeminiBodyAtPrefix patches the tools and generationConfig of a Gemini body
// rooted at prefix: "" for the bare body, "request." for the Antigravity envelope.
func sanitizeGeminiBodyAtPrefix(body []byte, prefix string) []byte {
	for i, tool := range gjson.GetBytes(body, prefix+"tools").Array() {
		if !tool.IsObject() {
			continue
		}

		for _, declKey := range []string{"functionDeclarations", "function_declarations"} {
			declarationsPath := fmt.Sprintf("%stools.%d.%s", prefix, i, declKey)

			for j, declaration := range gjson.GetBytes(body, declarationsPath).Array() {
				if !declaration.IsObject() {
					continue
				}

				body = sanitizeGeminiFunctionDeclaration(body, fmt.Sprintf("%s.%d", declarationsPath, j))
			}
		}
	}

	return sanitizeGeminiGenerationConfig(body, prefix)
}

// sanitizeGeminiGenerationConfig drops explicit null response schemas from the
// generationConfig block, which Gemini rejects.
func sanitizeGeminiGenerationConfig(body []byte, prefix string) []byte {
	for _, configKey := range []string{"generationConfig", "generation_config"} {
		configPath := prefix + configKey
		if !gjson.GetBytes(body, configPath).IsObject() {
			continue
		}

		for _, schemaKey := range geminiGenerationResponseSchemaKeys {
			body = deleteNullToolSchema(body, configPath+"."+schemaKey)
		}
	}

	return body
}

// sanitizeGeminiFunctionDeclaration enforces the Gemini oneof over the input schema keys
// of one function declaration and drops its null output schema keys.
func sanitizeGeminiFunctionDeclaration(body []byte, declPath string) []byte {
	body = sanitizeGeminiParameterSchema(body, declPath)

	for _, key := range geminiResponseSchemaKeys {
		body = deleteNullToolSchema(body, declPath+"."+key)
	}

	return body
}

// sanitizeGeminiParameterSchema collapses the input schema keys of declPath to a single
// non-null key. When a schema is already populated every null key is deleted; when all
// present keys are null the first is replaced by the empty object schema and the rest are
// deleted. Scanning before mutating keeps the result independent of key order.
func sanitizeGeminiParameterSchema(body []byte, declPath string) []byte {
	nullKeys := make([]string, 0, len(geminiParameterSchemaKeys))

	hasPopulated := false

	for _, key := range geminiParameterSchemaKeys {
		result := gjson.GetBytes(body, declPath+"."+key)

		switch {
		case !result.Exists():
		case result.Type == gjson.Null:
			nullKeys = append(nullKeys, key)
		default:
			hasPopulated = true
		}
	}

	if len(nullKeys) == 0 {
		return body
	}

	if !hasPopulated {
		// Every present key is null: keep the canonical first key and drop the rest.
		body = replaceNullToolSchema(body, declPath+"."+nullKeys[0], false)
		nullKeys = nullKeys[1:]
	}

	for _, key := range nullKeys {
		body = deleteNullToolSchema(body, declPath+"."+key)
	}

	return body
}
