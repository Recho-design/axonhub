# 函数工具参数 Schema 归一化契约

## 范围

`llm/tools.go` 的 `Function.EffectiveParameters()` / `NormalizeFunctionParameters()` / `NormalizeEmptySchema()` / `IsEmptyObjectSchema()`，`llm/tool_placeholder.go` 的占位注入，以及各 `llm/transformer/**` 出站与 `internal/server/orchestrator` 的统一链路、直通、覆写注入点。

关联：`.agents/notes/2026-11-23-tool-schema-null-normalization.md`

## 核心不变量

1. **任何出站请求不得出现 `"parameters": null` / `"parametersJsonSchema": null` / `"input_schema": null`。**
   唯一例外：未开启占位开关时，Responses 同协议往返中客户端**原本省略**的 `parameters` 继续保持省略（`omitempty` 行为），以维持 `TestNamespaceReview_RawNamespaceAndCatalogChange` 的字节往返契约。

2. **字段取值优先级**：`EffectiveParameters()` 先取可用的 `ParametersJsonSchema`，再取 `Parameters`；`ParametersJsonSchema` 为 `"{}"` 而 `Parameters` 有实质内容时让位于 `Parameters`。

3. **空对象模板不可变**：`EmptyObjectSchema` / `StrictEmptyObjectSchema` 是包级共享模板，所有访问器必须返回 `slices.Clone` 副本。调用方不得原地修改返回值。

4. **归一化只认四种无参形态**：nil、空白、字面 `null`、`{}`，以及**恰好是 `{"type":"object"}`（无其他键）**。
   其他 schema 一律返回原切片，不重编码、不补 `properties`。

5. **无参判定 `IsEmptyParametersSchema` 是唯一实现**（`llm/tool_placeholder.go`）。`llm.IsEmptyObjectSchema` 直接复用它。
   判定条件：`type` 缺省或为 `"object"`；`properties` 缺省/null/空对象；`additionalProperties` 缺省或 `false`；不含 `patternProperties`、`anyOf`、`oneOf`、`allOf`、`$ref`、`not`、`if`、`enum`、`const`。

6. **`strict: true` 的空对象形态**固定为
   `{"type":"object","properties":{},"required":[],"additionalProperties":false}`。

7. **响应 schema 不得补空对象**。Gemini/Google 明确拒绝 `Type.OBJECT` 无属性的响应 schema；`response_format` 的 `schema` 为空、`null` 或无属性对象时只能丢弃该字段（保留 `responseMIMEType: application/json`），必要时降级为 `json_object`。

## 无参工具占位注入（渠道开关）

老旧的 OpenAI 兼容反代会把 `{"type":"object","properties":{}}` 转成 Gemini 的 `parameters: null` 后自行返回 400
（`expected record, received null`）。渠道级开关 `TransformOptions.InjectEmptyToolPlaceholder` 打开时，给无参工具注入一个可选占位参数：

- 非 strict：`{"type":"object","properties":{"_placeholder":{"type":"boolean","description":"Placeholder. Leave unset."}}}`
- strict：`{"type":"object","properties":{"_placeholder":{"type":["boolean","null"],"description":"Placeholder. Always pass null."}},"required":["_placeholder"],"additionalProperties":false}`

注入点：

| 链路 | 入口 | 说明 |
|---|---|---|
| 统一请求 | `llm.InjectEmptyToolPlaceholders` ← `applyTransformOptions` | 复制 Tools 切片，不原地改原请求；对 `type=="function"` 且 `EffectiveParameters()` 判为无参的工具写入占位并清空 `ParametersJsonSchema` |
| 直通 body | `llm.InjectToolPlaceholders` ← `mergePassThroughRequestBody` 之后的 `injectToolPlaceholders` | 无改写时返回原字节，保护 Prompt Cache |
| Responses 传输适配 | 同上，`applyResponsesTransportRequestBody` 内 | 在 `AdaptRequestBody` + `mergePassThroughRequestBody` 之后 |
| Response 续写 | 不单独注入 | 只拷贝 `input` / `previous_response_id`，工具来自已注入的 outbound body |
| 覆写 body | `llm.InjectToolPlaceholders` ← `applyOverrideRequestBody` 末尾 | 覆写操作完成后作用于最终 body |

原始请求体注入对各协议的路径：

| 协议 | 路径 | 触发条件 |
|---|---|---|
| OpenAI Chat / Ollama | `tools[].function.parameters` | `type=="function"` 或 `function` 存在；字段缺失也算无参 |
| OpenAI Responses | `tools[].parameters` | 仅 `type=="function"`；字段缺失也算无参（开关为显式开启，不影响默认的省略往返契约） |
| Anthropic | `tools[].input_schema` | 仅普通自定义工具（无 `type` 或 `type=="custom"`）；字段缺失也算无参 |
| Gemini | `tools[].functionDeclarations[]` 与 `function_declarations[]` 的 `parameters` / `parametersJsonSchema` / `parameters_json_schema` | 三者均非可用参数时写入原本存在的键（都不存在写 `parametersJsonSchema`），并删除另一个为 `null` 的键 |

## 各协议字段映射

| 协议 | 入参字段 | 响应 schema 字段 |
|---|---|---|
| OpenAI Chat | `tools[].function.parameters` | `response_format.json_schema.schema` |
| OpenAI Responses | `tools[].parameters` | `text.format.schema` |
| Anthropic | `tools[].input_schema` | — |
| Gemini | `functionDeclarations[].parameters` / `parametersJsonSchema` / `parameters_json_schema`（oneof） | `generationConfig.responseSchema` / `responseJsonSchema` |
| Ollama | `tools[].function.parameters` | — |
| Antigravity | 外层 `request.tools[].functionDeclarations[]`，只接受大写类型的 `parameters` | `request.generationConfig.responseSchema` |

## Gemini `oneof` 三键互斥

`parameters`、`parametersJsonSchema`、`parameters_json_schema` 至多允许一个存活。

## Antigravity 特别约束

- 必须实现 `PassThroughBodyPolicy.AllowPassThroughBody` 返回 `false`：其真实请求体外包 `{"project","model","request"}` 信封，原始 Gemini body 直通会破坏信封结构。
- 工具参数只接受 `parameters`（大写类型值），`parametersJsonSchema` 必须无条件清空；`ResponseJsonSchema` 无条件转成 `ResponseSchema`。
- 参数 schema 取值：`ParametersJsonSchema` 不可用则取 `Parameters`，仍为空用空对象模板。

## 既有测试契约（不可破坏）

- `llm/transformer/openai/responses/namespace_roundtrip_test.go`：Responses 同协议往返保持 `tools` 语义一致（省略的 `parameters` 不补字段）；`namespace` 的 `description` / `defer_loading` / `custom` 子工具原样保留。
- `llm/transformer/gemini/convert_test.go`：`"parameters": null` 与正常 schema 混合时，两者都产出合法对象 schema。
- `internal/server/orchestrator/pass_through_test.go`：无改写需求的请求体字节完全不变（`TestMergePassThroughBodyLeavesNormalGeminiBodyUntouched`）。
- `llm/transformer/openai/responses/outbound_test.go`：`convertFunctionToTool` 不为带约束的 schema 强塞 `properties`。
