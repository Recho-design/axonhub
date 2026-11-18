# OpenAI Chat SSE 终帧排序保证（finish_reason 与增量解耦）

日期：2026-11-18
关联：`llm/transformer/openai/inbound_stream.go`、`llm/transformer/gemini/outbound_stream.go`

## 背景

`gemini_chat` 上游在单轮并行工具调用时，`finishReason` 会与 `functionCall` parts 同 chunk（或提前）到达。
网关原样透传导致 `finish_reason: "tool_calls"` 挂在携带 `delta.tool_calls` 的 SSE 帧上、甚至提前发送，
后续增量落在 finish 之后，严格客户端（`@opencode/ai` / OpenCode v2）报
`InvalidProviderOutputError: received content after the finish reason` 中断。

## 方案（收敛于"入站严格屏障"）

修复层选在 `openAIInboundStream`（所有上游格式 → OpenAI Chat SSE 的最后一层状态流）：

1. **所有非空 finish_reason 一律 defer**（`pendingFinish`），包括上游自发的 delta-free 终帧——
   因为病态上游可能在空终帧之后继续发同 choice 的增量（透传会完整重现本 bug）。
2. **rank 合并**：`length/content_filter(2) > tool_calls/function_call(1) > stop(0)`，
   后到的通用原因不得覆盖先到的特异原因（防止截断原因 `length` 被流尾 `stop` 降级丢失）。
3. **usage 暂存重排**：有挂起 finish 时扣留 usage，流末按 `finish → usage(choices:[]) → [DONE]` 排空，
   符合 `stream_options.include_usage` 规范形状。
4. **错误路径按完整性校验补发**（`verifyChoiceComplete`）：仅当该 choice 聚合输出完整
   （工具 name 非空且 arguments 为合法 JSON）才在错误帧前补发挂起的 finish——
   既保留 `stream_lifecycle_test` 的既有契约（choice 已终结后断流仍保留 finish），
   又不让残缺 turn 被客户端误判为可执行。
5. 剥离 finish/usage 后无 payload 的帧整帧扣留（不输出空噪声帧）。

## 被放弃方案及原因

- **gemini outbound 层剥离 finish**：统一流会丢失真实映射（MAX_TOKENS→length 会被下游合成逻辑
  降级为 stop/tool_calls），且只修 Gemini 一家，其他 OpenAI 兼容上游同形状问题不修。
- **空终帧透传 + finishEmitted 标记**（第一版实现，头脑风暴评审推翻）：存在两个可证伪漏洞——
  空终帧放行后同 choice 增量逃逸（重现 bug）；挂起的 `length` 被后续空终帧 `stop` 覆盖。
- **SSE 写出层修**：该层不解析 JSON 增量结构，且会影响 Anthropic 等其他入站。
- **整轮聚合再分段输出**：TTFB 退化成非流式，毁灭性倒退。

## 已知欠债（未处理，需单独验证）

Anthropic `/v1/messages` 与 OpenAI `/v1/responses` 入站是否有同类"finish 早于增量"问题未验证
（各自有独立流状态机：anthropic inbound 聚合 toolCalls 后发 message_delta；responses inbound
有自己的 terminal 事件处理）。若 Gemini 并行工具调用在这两类客户端也出问题，应考虑把
defer 逻辑下沉为统一流的公共 Stream Wrapper。
