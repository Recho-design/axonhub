# 函数工具参数 Schema 的 null 归一化与跨协议互通

2026-11-23

## 背景

上游 Gemini 反代（oai-proxy）返回 400：

```
Request validation failed. At 'tools > [0] > functionDeclarations > [0] > parameters':
Invalid input: expected record, received null
```

根因不是单点，而是三类问题叠加：

1. 无参工具在客户端侧常被写成 `"parameters": null` 或直接省略字段。
2. 各协议对"无参函数"的容忍度不同：Gemini 官方允许省略 `parameters`，但套了 Zod/TypeBox 校验的反代把它物化成 `null` 后拒绝；OpenAI Responses 的 `parameters` 字段在 `omitempty` 下会被整个吞掉。
3. Gemini 有 `parameters`（OpenAPI 3.0 子集）与 `parametersJsonSchema`（Draft 2020-12）双轨字段，且互斥。此前只有 Gemini/Antigravity 出站读新字段，OpenAI/Anthropic/Responses/Ollama 出站全只读旧字段，导致 Gemini 新格式入站转其它协议时参数定义整段丢失。

## 方案

- `llm/tools.go` 统一出口：新增 `Function.EffectiveParameters()` 打通双轨字段；`NormalizeFunctionParameters` 负责空值归一，并新增 `IsEmptyObjectSchema` 供响应 schema 场景复用。
- 空壳识别采用**白名单 + 只读探测**：仅当对象只含 `type/properties/required/additionalProperties` 与纯元数据键，且 `type` 缺省或为 `"object"`、`properties` 缺失/`null`/空、`required` 缺失/`null`/空、`additionalProperties` 缺失或 `false` 时才补 `properties: {}`。
- 已完整的空对象 schema（客户端自带键序、缩进、`additionalProperties: false`）直接返回原切片，不重编码。
- 全局模板 `EmptyObjectSchema` / `StrictEmptyObjectSchema` 一律 `slices.Clone` 返回，防止调用方原位修改污染后续请求。
- 直通/覆写层新增 `sanitizeNullToolSchemasInRawBody`：只用 `gjson` 探测，仅当目标路径**确实存在且为 `json.Null`** 时才 `sjson` 改写；正常请求零字节变动，保住 Prompt Cache。

## 被放弃的方案及原因

1. **全量 AST 重序列化（所有 schema 统一 Unmarshal→改写→Marshal）**
   放弃。Go 的 `map[string]any` 序列化会按字母序重排键，Anthropic/OpenAI/Gemini 的隐式前缀缓存强依赖 `tools` 段的字节确定性，重排等于打穿用户的缓存命中率与账单。改为"只在确需修补时改那几个字节"。

2. **在 Responses 出站对所有无参工具无条件补 `parameters`**
   放弃。项目既有契约 `TestNamespaceReview_RawNamespaceAndCatalogChange` 要求 Responses→Responses 时，客户端原本省略 `parameters` 的工具必须原样往返（`JSONEq(original["tools"], body["tools"])`）。改为：省略则保持省略（`strict: true` 除外），只有显式 `null` / `{}` / 跨协议来源才补。

3. **把 `{}` 和 `{"type":"object"}` 一律视为"无参"并补 `properties: {}`**
   放弃。`{"type":"object","additionalProperties":{"type":"string"}}` 是合法的动态字典入参，强补 `properties: {}` 会让 Antigravity 的 `addEmptySchemaPlaceholder` 误判为无参函数并注入 `_placeholder`，直接改写调用语义。

4. **在 Pass-Through 里直接重写整个请求体**
   放弃。Pass-Through 的存在意义就是字节级复用原始请求以命中前缀缓存。改为仅对显式 `null` 做外科手术式修补。

5. **用 Override 中间件充当全链路兜底**
   放弃。`applyOverrideRequestBody` 在渠道未配置任何 override 时会提前返回，无法兜住转换器自身漏出的 `null`。改为在转换器内收口 + 直通/覆写两处独立兜底。

## 踩坑与易回归点

- **Gemini `oneof` 是三键而非两键**：`parameters` / `parametersJsonSchema` / `parameters_json_schema` 同时存在时，逐键循环修补会因前一轮改动污染后一轮的判空状态，导致修补后仍有两个非 null 字段。必须"先全量扫描、再统一变更"。
- **Antigravity 有外层 `request` 信封**：其 `APIFormat()` 与 Gemini 相同，但真实请求体是 `{"project":..,"model":..,"request":{...}}`。因此（a）必须实现 `PassThroughBodyPolicy` 拒绝原始 body 直通，否则信封被整个覆盖；（b）修补需同时覆盖 `request.tools` 与 `request.generationConfig`。
- **`len(rawMessage) > 0` 无法区分 `null`**：`json.RawMessage("null")` 长度为 4。所有"字段是否存在"的判断必须走 `bytes.TrimSpace` + 字符串比较，或 `gjson` 的 `Exists()` / `Type == gjson.Null`。
- **Gemini 入站的 `null` 会被解成布尔 false schema**：`google/jsonschema-go` 把 JSON `null` 反序列化为 `false`，序列化后成为 `{"not":{}}`，语义从"无参"变成"拒绝一切输入"。入站必须在进 `xjson.Transform` 之前拦截。
- **`responseJsonSchema` 不能补空对象**：Google 官方规定 `Type.OBJECT cannot be empty; it must contain other properties`。响应 schema 的空/`null` 只能丢弃字段，不能补 `{"type":"object","properties":{}}`。
- **Responses `namespace` 工具会被 `RawTools` 回放覆盖**：`isStructurallyRepresentedToolType` 未包含 `"namespace"`，原始碎片在 `mergeRawOnlyTools` 中覆盖了结构化重建的结果，导致清洗失效。必须在保存/读取原始碎片时都做一次显式 `null` 清洗。
- **Responses HTTP ↔ WebSocket 传输适配会重排顶层键**：`AdaptRequestBody` 走 `map[string]json.RawMessage` 往返，属既有行为，本次未改动，但它确实破坏顶层字节序，属已知的缓存退化风险点。
