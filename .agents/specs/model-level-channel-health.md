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
- 候选整体健康：仅当候选内**所有**可尝试模型都不健康时，才把候选判为不健康/降权；任一健康模型存在时不得整体降分。
- ErrorAware 与 RoundRobinHealth 优先读候选当前模型指标；无模型上下文/提供者不支持时回退渠道级指标。
- failover 与 round-robin 的健康过滤器都使用上述候选级判定；adaptive 通过 ErrorAware 软降权。

## AttemptSeq 所有权

- `PersistentOutboundTransformer.TransformRequest` 每次 attempt 开始时递增 `PersistenceState.AttemptSeq`。
- `performanceRecording.OnOutboundRawRequest` 创建 `PerformanceRecord` 时写入当前 `AttemptSeq`。
- `OnOutboundRawError` 仅在 `perf.AttemptSeq == state.AttemptSeq` 时记录；否则视为上一 attempt 的陈旧记录。不要用 `RequestCompleted` 判断，否则会吞掉非流式内容错误/空响应对同一 attempt 成功终态的覆盖。

## 非目标（保持渠道级/既有行为）

- auto-disable、渠道状态、请求计数、延迟 EWMA、RateLimit 保持渠道级。
- ModelCircuitBreaker 仍使用 `(channelID, requestedModel)`，不随本约定改为 ActualModel。
- 不新增配置项、GraphQL 字段或前端 UI。
