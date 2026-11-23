# 函数工具参数 Schema 归一化契约

## 范围

`llm/tools.go` 的 `Function.EffectiveParameters()` / `NormalizeFunctionParameters()` / `NormalizeEmptySchema()` / `IsEmptyObjectSchema()`，以及各 `llm/transformer/**` 出站、`internal/server/orchestrator/pass_through_tool_schema.go` 的直通与覆写修补。

关联：`.agents/notes/2026-11-23-tool-schema-null-normalization.md`

## 核心不变量

1. **任何出站请求不得出现 `"parameters": null` / `"parametersJsonSchema": null` / `"input_schema": null` / `"responseSchema": null` / `"response": null`。**
   唯一例外：Responses 同协议往返时，客户端**原本省略**的 `parameters` 继续保持省略（`omitempty` 行为），以维持 `TestNamespaceReview_RawNamespaceAndCatalogChange` 的字节往返契约。

2. **字段取值优先级**：`EffectiveParameters()` 先取可用的 `ParametersJsonSchema`，再取 `Parameters`；`ParametersJsonSchema` 为 `"{}"` 而 `Parameters` 有实质内容时让位于 `Parameters`。

3. **空对象模板不可变**：`EmptyObjectSchema` / `StrictEmptyObjectSchema` 是包级共享模板，所有访问器必须返回 `slices.Clone` 副本。调用方不得原地修改返回值。

4. **空壳判定为白名单制**。仅当对象同时满足以下条件才补 `properties: {}`：
   - 所有键都在 `{type, properties, required, additionalProperties, description, title, $schema, $id, $comment, strict, examples, default}` 内；
   - `type` 缺省或为字符串 `"object"`；
   - `properties` 缺省、为 `null` 或空对象；
   - `required` 缺省、为 `null` 或空数组；
   - `additionalProperties` 缺省或为 `false`。

   含 `additionalProperties`（对象或 `true`）、`patternProperties`、`anyOf`/`oneOf`/`allOf`、`$ref`、`not`、`if`、`enum`、`const` 等任一约束键的 schema **必须原样返回原切片**。

5. **已完整的 schema 不重编码**。同时具备 `"type":"object"` 与空 `properties` 对象（strict 下还需 `additionalProperties: false` 与 `required: []`）的输入，直接返回原 `json.RawMessage`，保留客户端键序、缩进与自定义约束。

6. **`strict: true` 的空对象形态**固定为
   `{"type":"object","properties":{},"required":[],"additionalProperties":false}`。

7. **`required: null` 必须清除**，不得随归一化结果继续下发。

8. **响应 schema 不得补空对象**。Gemini/Google 明确拒绝 `Type.OBJECT` 无属性的响应 schema；`response_format` 的 `schema` 为空、`null` 或空壳时只能丢弃该字段（保留 `responseMIMEType: application/json`），必要时降级为 `json_object`。

## 各协议字段映射

| 协议 | 入参字段 | 响应 schema 字段 |
|---|---|---|
| OpenAI Chat | `tools[].function.parameters`、遗留 `functions[].parameters` | `response_format.json_schema.schema` |
| OpenAI Responses | `tools[].parameters`、`tools[].tools[].parameters`（namespace） | `text.format.schema` |
| Anthropic | `tools[].input_schema` | — |
| Gemini | `functionDeclarations[].parameters` / `parametersJsonSchema` / `parameters_json_schema`（oneof） | `generationConfig.responseSchema` / `responseJsonSchema`、声明内 `response` |
| Ollama | `tools[].function.parameters` | — |
| Antigravity | 外层 `request.tools[].functionDeclarations[]`，只接受大写类型的 `parameters` | `request.generationConfig.responseSchema` |

## Gemini `oneof` 三键互斥

`parameters`、`parametersJsonSchema`、`parameters_json_schema` 至多允许一个存活：

- 若已存在任一非 `null` 值：删除其余所有显式 `null` 键。
- 若存在的键全为 `null`：把第一个替换为 `EmptyObjectSchema`，删除其余。

实现必须"先扫描全部键的状态，再统一变更"，禁止逐键边扫边改（前一键的改写会污染后一键的判空结果）。

## 直通与覆写修补

`internal/server/orchestrator/pass_through_tool_schema.go`：

- 只在目标字段**存在且值为 `json.Null`** 时改写；字段缺失或已是对象时一个字节都不动（保护 Prompt Cache）。
- 修补点位于 `mergePassThroughRequestBody` 中 `passThroughBodyNeedsModelPatch` 判断**之前**（Gemini 的 model 在 URL 里，该函数会提前返回）。
- 覆盖 `applyPassThroughRequestBody`、`applyResponsesTransportRequestBody` 与 `applyOverrideRequestBody`。
- 同时兼容 Gemini 驼峰/下划线键名与 Antigravity 的 `request.` 信封前缀。

## Antigravity 特别约束

- 必须实现 `PassThroughBodyPolicy.AllowPassThroughBody` 返回 `false`：其真实请求体外包 `{"project","model","request"}` 信封，原始 Gemini body 直通会破坏信封结构。
- 工具参数只接受 `parameters`（大写类型值），`parametersJsonSchema` 必须无条件清空。
- 空对象 schema 经 `SanitizeJSONSchema` 会注入 `_placeholder` 必填布尔参数。

## 既有测试契约（不可破坏）

- `llm/transformer/openai/responses/namespace_roundtrip_test.go`：Responses 同协议往返保持 `tools` 语义一致（省略的 `parameters` 不补字段）；`namespace` 的 `description` / `defer_loading` / `custom` 子工具原样保留。
- `llm/transformer/gemini/convert_test.go`：`"parameters": null` 与正常 schema 混合时，两者都产出合法对象 schema。
- `internal/server/orchestrator/pass_through_test.go`：无 `null` 的请求体字节完全不变。
