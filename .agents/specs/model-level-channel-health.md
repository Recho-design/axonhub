# 模型级渠道健康约定

适用范围：`internal/server/biz/channel_metrics.go` 的模型健康指标、`internal/server/orchestrator` 的候选排序与健康策略。仅约束重试/路由相关逻辑。

## 健康键与记录

- 健康键为 `(channelID, ActualModel)`：渠道 ID + 渠道映射后的上游实际模型；不是 AxonHub 聚合模型 `Model.ModelID`。
- `PerformanceRecord.Model` 记录本次 attempt 的上游实际模型；`PerformanceRecord.AttemptSeq` 记录 attempt 代数。
- 成功：该模型连续失败清零，保留 `LastFailureAt`（与渠道级 ErrorAware 语义一致）。
- 失败：连续失败 +1，`LastFailureAt` 取最大完成时间。
- 取消与空模型：不写入模型健康。
- 启动恢复：从 `request_execution.model_id` 重建窗口内模型级连续失败与最近失败；空模型行跳过。渠道级恢复逻辑保持不变。
- `GetChannelModelMetrics(ctx, channelID, modelID)` 返回隔离快照；找不到返回空快照。

## 候选排序与策略

- `DefaultSelector.selectModelCandidates` 在候选生成后按模型健康稳定重排 `Models`：近期失败模型后置；同步重排 `modelAPIFormats` 并更新 `candidate.APIFormat`。`modelAPIFormats` 与 `Models` 长度不一致时跳过重排。
- “近期失败”判定：`ConsecutiveFailures > 0` 且 `LastFailureAt` 在 `roundRobinHealthCooldown`（5 分钟）内；无时间戳视为近期失败。
- 每候选 metrics 快照由 `LoadBalancer.candidateMetrics(ctx, candidate)` 组合：渠道级快照的负载字段（RequestCount、延迟 EWMA）+ 候选首模型（`healthModelOf`）的 `ConsecutiveFailures`/`LastFailureAt`。策略通过 `Score/ScoreWithDebug` 的 metrics 参数读取，策略自身不再持有 provider。
- 候选整体健康：模型重排保证首模型是候选内最健康的；组合快照携带首模型健康，`RoundRobinHealthStrategy.IsUnhealthy(metrics)` 对候选的判定等价于“重排后首模型不健康”。任一健康模型存在时不得整体降分。
- ErrorAware 与 RoundRobinHealth 优先读组合快照中的模型级健康；provider 不支持模型级查询或候选无模型时回退渠道级字段。
- failover 与 round-robin 的健康过滤器都使用上述组合快照判定；adaptive 通过 ErrorAware 软降权。
- 会话粘性（`PREFER_PREVIOUS_CHANNEL`）置顶前同样通过 `candidateMetrics` + `RoundRobinHealthStrategy.IsUnhealthy` 校验粘性候选首模型健康；不健康时不予粘性置顶（trace 不健康时可回退至健康的 thread 粘性候选，均不健康则交由常规负载均衡排序）。

## AttemptSeq 所有权

- `PersistentOutboundTransformer.TransformRequest` 每次 attempt 开始时递增 `PersistenceState.AttemptSeq`。
- `performanceRecording.OnOutboundRawRequest` 创建 `PerformanceRecord` 时写入当前 `AttemptSeq`。
- `OnOutboundRawError` 仅在 `perf.AttemptSeq == state.AttemptSeq` 时记录；否则视为上一 attempt 的陈旧记录。不要用 `RequestCompleted` 判断，否则会吞掉非流式内容错误/空响应对同一 attempt 成功终态的覆盖。

## 流终态发布

- `OutboundPersistentStream.finalizeClose` 的每个终态出口都必须经 `finishFinalization()`：先 `publishPerformance`，再关闭 source。
- 发布必须晚于 `persistResponseChunks` 中的内容错误检测。终端事件只代表上游协议结束，内容错误检测会把同一 attempt 改判为失败；若在终端事件处理时就发布，worker 记下的是“成功”，模型级连续失败不会累加，错误渠道会持续被选中。
- 不要在各分支里散落发布调用，否则新增出口容易漏发；集中到 `finishFinalization()` 才能保证每个终态出口都发布。
- 发布按 `(AttemptSeq, Success)` 去重并对记录做快照：流终态与 pipeline 的 `OnOutboundRawError` 可能观察到同一 attempt（例如 `preReadLlmStream` 先关闭流再上报错误，或 auto-aggregate 的透传内容错误），同结局重复发布只计一次。成功→失败的改判仍会再次发布，保留非流式内容错误对成功终态的覆盖语义。

## 非目标（保持渠道级/既有行为）

- auto-disable、渠道状态、请求计数、延迟 EWMA、RateLimit 保持渠道级。
- ModelCircuitBreaker 仍使用 `(channelID, requestedModel)`，不随本约定改为 ActualModel。
- 不新增配置项、GraphQL 字段或前端 UI。
