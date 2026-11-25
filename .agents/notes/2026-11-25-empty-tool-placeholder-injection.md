# 无参工具占位参数注入（取代此前的 null 修补）

2026-11-25

## 背景

用户的 OpenAI Chat Completions 渠道上游是老旧反代：它把无参工具
`{"type":"object","required":[],"properties":{}}` 转成 Gemini 时写成
`parameters: null`，然后自己 400：

```
At 'tools > [0] > functionDeclarations > [0] > parameters': Invalid input: expected record, received null
```

关键在于请求体本身**没有 null**，且常走同协议直通（完全绕过 transformer）。
因此此前提交 e4c82a53 的猜测性补丁（只在目标字段是 `json.Null` 时改写）
对真实问题无效——null 是**上游反代自己**制造的。

## 方案

1. 新增渠道开关 `TransformOptions.InjectEmptyToolPlaceholder`。开启时给无参工具
   注入一个可选占位参数，让 schema 里始终有真实 property，反代不会再把它转成 null：
   - 非 strict：`_placeholder`（`boolean`，可选）
   - strict：`_placeholder`（`["boolean","null"]`）+ `required:["_placeholder"]` + `additionalProperties:false`
2. 三个注入点覆盖所有链路：统一请求（`applyTransformOptions`）、直通原始 body
   （`applyPassThroughRequestBody` / `applyResponsesTransportRequestBody`）、
   覆写后的 body（`applyOverrideRequestBody`）。原始 body 注入只在需要时改写字节。
3. `IsEmptyParametersSchema` 成为唯一的无参判定，`IsEmptyObjectSchema` 复用它。
4. 删除 e4c82a53 中无效的部分：`pass_through_tool_schema.go` 整个文件、
   `sanitizeRawToolFragment`、`extractJSONSchema` 的 name 启发式、
   Antigravity 的 `isObjectJSON` / `usableAntigravityResponseSchema` 双重回退、
   以及 `llm/tools.go` 的空壳装饰机。

## 被放弃的方案及原因

1. **继续在直通层修补 `json.Null`**
   放弃。真实请求体没有 null，修补点永远不触发；问题在上游，不在字节流。
2. **对所有无参工具无条件注入占位**
   放弃。占位参数会改变模型看到的工具契约（多一个字段），必须由用户按渠道显式开启。
3. **统一请求链路就地修改 `req.Tools`**
   放弃。请求可能被重试复用；改为复制切片后再改形态，与原请求隔离。

## 保留的真实修复

- `EffectiveParameters` 跨协议双轨字段互通。
- Gemini 入站 `null → nil` 拦截（`jsonschema-go` 会把 null 解成 `{"not":{}}`）。
- Anthropic `input_schema` 兜底、Ollama / OpenAI Chat 不发 `parameters: null`。
- Antigravity `AllowPassThroughBody` 返回 false（信封结构）与 `ResponseJsonSchema → ResponseSchema` 转换。
- Responses 同协议省略参数保持省略（`TestNamespaceReview_RawNamespaceAndCatalogChange`）；仅在显式开启占位开关时，缺失的 `parameters` 才视为无参并注入占位。
- Gemini 拒绝空对象响应 schema，只丢弃字段不补对象。
