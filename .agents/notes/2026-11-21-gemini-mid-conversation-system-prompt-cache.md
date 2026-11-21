# Gemini 出站转换仅提取前导 system/developer 消息以保护隐式前缀缓存

日期：2026-11-21
关联：`llm/transformer/gemini/outbound_convert.go`、`llm/transformer/gemini/convert.go`、`llm/transformer/gemini/outbound_convert_test.go`

## 背景

`llm/transformer/gemini` 在将统一 `llm.Request` 转换为 Google Gemini / Vertex AI `GenerateContentRequest` 时，原先在 `convertLLMToGeminiRequestWithConfig` 中遍历全量 `chatReq.Messages`，将任意位置出现的 `system` 与 `developer` 消息全部提取并追加到顶层 `systemInstruction`（位于 Token 0）。

当下游客户端（如 SillyTavern 的动态世界书触发、前情摘要更新、深度注入 `@D` 提示词等）在对话中后段插入 `system`/`developer` 消息时，每次新增或变动都会改写请求最开头的 `systemInstruction`，直接导致后续整段历史上下文在 Google Gemini / Vertex AI 上的隐式前缀缓存（Implicit Prompt Caching）失效。

## 方案

参考 SillyTavern（`convertGooglePrompt`）、CLIProxyAPI 与 LiteLLM（PR #41493 / Issue #42104）的主流边界与降级实现，收敛为以下规则：

1. **连续前导布尔锁（`hasEncounteredConversation`）**：
   - 在 `convertLLMMessagesToGemini` 中从头遍历 `messages`，仅在 `!hasEncounteredConversation` 时将连续前导的 `system`/`developer` 文本块提取合并至顶层 `systemInstruction`。
   - 一旦遇到第一条非 `system`/`developer` 消息（包括 `user`、`assistant`、`tool` 等，即使该消息如空 `user("")` 转换结果为 `nil`），立即将 `hasEncounteredConversation` 置为 `true` 且不可逆。
   - 保持既有纯系统消息契约：若请求仅包含前导 `system`/`developer` 消息，`systemInstruction` 正常填充而 `contents` 为空，交由 `TransformRequest` 返回 `transformer.ErrInvalidRequest`（与 Issue #2505 一致）。
2. **中后段原地降级为 `role: "user"`**：
   - Gemini `contents` 协议仅允许 `"user"` 与 `"model"` 角色；在 `convertLLMRoleToGeminiRole` 中将 `"system"` 与 `"developer"` 统一映射为 `"user"`。
   - 对话开始后的 `system`/`developer` 消息经 `convertLLMMessageToGeminiContent` 原地转为 `role: "user"` 的 `Content`，不包裹额外标签前缀，也不强制合并相邻 `user` 轮次。
3. **工具轮次防夹心顺延（`hasUpcomingToolMessage`）**：
   - 若中段 `system`/`developer` 消息恰好夹在 `assistant(tool_calls)` 与后续 `tool` 响应之间（或夹在多条连续 `tool` 响应中间），先暂存至 `deferredSystemContents`；待该组连续 `tool` 响应全部合并进紧随 `model` 之后的同一个 `FunctionResponse` `Content` 后再按序追加，避免打断 Gemini 要求的 `functionCall` → `functionResponse` 紧邻配对。
4. **多模态文本块空字符串过滤**：
   - `convertLLMMessageToGeminiContent` 处理 `MultipleContent` 的 `"text"` 分支补齐 `*part.Text != ""` 校验（与 `extractPartsFromLLMMessage` 对齐），防止空字符串生成空 `{}` `Part` 触发上游 `400 INVALID_ARGUMENT: Part must have a value`。

## 被放弃方案及原因

- **以 `len(contents) == 0` 代替 `hasEncounteredConversation` 判定开头**：当首条 `user` 消息内容为空字符串时，`convertLLMMessageToGeminiContent` 返回 `nil`，`contents` 仍为空，会导致紧随其后的中段 `system` 被误判为前导消息提拔进 `systemInstruction`。
- **为降级后的中段 `system` 包裹 `<system-reminder>` 等伪标签**：CLIProxyAPI 采用了该包装以适配编程 Agent，但对 SillyTavern 等酒馆类客户端的世界书、`@D` 深度提示词而言属于篡改提示词原文，易干扰角色扮演指令效果。
- **强制将降级后的 `user` 与相邻 `user` 轮次用 `\n\n` 合并**：Gemini 2.0/2.5/3 原生支持连续多个 `user` `Content` 节点；保持独立 `Content` 能保留原始消息边界与多模态结构，改动面更小。
- **本次一并改动 Anthropic 转换器**：`llm/transformer/anthropic` 虽同样存在全局提取 `system` 的行为，但 Anthropic 依赖显式 `cache_control` 断点机制，缓存语义与 Gemini 隐式前缀缓存不同，留待后续独立评估。
