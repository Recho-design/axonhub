# 流终态性能记录发布丢失（内容错误检测失效）

## 背景

开启“返回文本错误”（`RetryPolicy.ContentErrorDetection`）后，上游以 HTTP 200 流式返回错误文本：请求执行记录被正确标记为 failed，但网关在下一次请求仍反复选中同一个坏渠道/渠道模型。按预期，模型级连续失败累加后应被 `RoundRobinHealth` / `ErrorAware` 降权。

## 根因

`internal/server/orchestrator/outbound.go` 的 `OutboundPersistentStream.finalizeClose` 在重写后只在“无终止事件的聚合完成”两个分支里调用 `enqueueCompletedPerformance`。最常见的正常流式路径（OpenAI `[DONE]`、Anthropic `message_stop` 等终止事件）直接 `closeSource()` 返回，性能记录从未送到 `ChannelService.RecordPerformance`。

内容错误检测 `detectStreamContentError` 在 `persistResponseChunks` 里把 `PerformanceRecord` 改为失败，但发布缺失使 `recordChannelModelPerformance` 不累加 `consecutiveFailures`，模型健康始终为 0，负载均衡持续认为该渠道健康。流式成功也不会发布，因此成功清零同样丢失。

该发布点最初由 `966c4f7e` 引入为 `finishFinalization()`（所有出口统一发布，且在内容错误检测之后）。后续上游提交 `984394c0` 重构 finalize 流程时把它替换为行内 `enqueueCompletedPerformance`，合并后 fork 的 `finishFinalization` 丢失，未再恢复。

## 方案

恢复 `finishFinalization()`：统一先 `publishPerformance`，再关闭 source；`finalizeClose` 的四个终态出口（终止事件、流错误、不完整、正常完成）全部走它，删除两个行内发布点。

顺序保证：`persistResponseChunks` → 聚合 → `detectStreamContentError`（改判失败）→ `finishFinalization` 发布，worker 观测到最终失败态。

## 双计防护（复盘补足）

仅恢复发布会让同一 attempt 出现两条发布来源：

- `preReadLlmStream` 在读取首包失败时先 `llmStream.Close()`（流终态发布一次），再由 `applyRawErrorResponseMiddlewares` 调用 `OnOutboundRawError` 发布第二次。
- auto-aggregate + 透传时，`applyInboundRawResponseMiddlewares` 的内容错误先经 `OnOutboundRawError` 发布，随后 `inboundStream.Close()` 的终态发布再来一次。

因此发布统一收敛到 `publishPerformance`：

- 按 `(AttemptSeq, Success)` 去重；同 attempt 同结局只入队一次。
- 结局从成功改判为失败时仍会再次发布，保留非流式内容错误对成功终态的覆盖语义。
- 入队的是记录快照，避免异步 worker 读到后续改判而把已提交的结局改写。

## 会话粘性健康门控

`TraceStickyMode = PREFER_PREVIOUS_CHANNEL` 不再把上次渠道无条件钉在候选首位。`LoadBalancedSelector.resolveStickyChannelID` 在返回粘性渠道前，用 `extractStickyCandidate` 取出该渠道优先级最高的候选，再经 `candidateMetrics` + `RoundRobinHealthStrategy.IsUnhealthy` 校验候选首模型健康（与常规排序同一判定）。不健康时：

- trace 上次渠道不健康：继续尝试 thread 上次渠道；thread 渠道在候选中且健康才返回。
- thread 渠道也不在候选中或不健康：返回 `0, false`，请求落入常规 `sortCandidates`。
- 无 `metricsProvider`、查询出错或快照为 nil 时 fail-open，保持既有行为。

健康门控在 `quotaGate.Filter` 之前解析，因此不健康渠道不会拿到 `stickyOnly` 配额豁免。

## 被放弃的方案

- **只在终止事件分支补一次发布**：能修当前症状，但错误/不完整出口仍然漏发，且未来新增出口容易再次漏。
- **在终端事件处理时发布**：会先发布成功，内容错误检测随后改判失败但不再发布；且与 worker 并发读写同一指针，存在竞态。
- **用 `RequestCompleted` 门控 `OnOutboundRawError` 去重**：spec 已禁止——会吞掉非流式内容错误/空响应对同一 attempt 成功终态的覆盖。
- **无条件幂等锁（先到先发）**：会拦截 auto-aggregate 中“失败后改判”的第二次发布，或吞掉成功后的失败覆盖；必须允许结局变化。
- **把发布挪到 attempt 执行栈顶层 `defer`**：SSE 流的消费跨出同步调用栈，改动半径过大，不适合热修。

## 验证

- `TestOutboundPersistentStream_TerminalEventPublishesPerformance`：终止事件流 Close 后性能记录必须到达渠道服务（以预置失败被成功清零为可观测信号）。
- `TestOutboundPersistentStream_ContentErrorPublishesFailure`：内容错误把同一次 attempt 改判失败后，模型级 `ConsecutiveFailures` 必须为 1。
- `TestPerformancePublish_CountsCollidingAttemptOnce`：终态发布与 `OnOutboundRawError` 观察到同一 attempt 时只能计 1 次失败。
- `TestPerformancePublish_KeepsSuccessThenFailureOverride`：成功→失败改判仍会发布，失败计数为 1。
- 上述用例逐一验证过：移除对应修复后失败，恢复后通过。`go test ./internal/server/orchestrator ./internal/server/biz` 全部通过。
- 会话粘性健康门控由 `TestLoadBalancedSelector_TraceStickySelection` 的回归用例覆盖：不健康的 trace 粘性渠道回退到常规排序、回退到健康的 thread 粘性渠道、以及首模型健康的多模型候选仍保留粘性。

## 遗留观察（未在本次修复）

- `detectStreamContentError` 位于 `persistResponseChunks` 的 `requestExec != nil` 分支内。生产路径的 `persistRequestExecution` 中间件总是为每次 attempt 创建执行记录，因此当前不会失效；但把“渠道健康检测”耦合到“是否持久化执行记录”仍然脆弱，后续可解耦。
