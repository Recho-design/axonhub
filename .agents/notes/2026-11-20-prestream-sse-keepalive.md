# 流式请求首包前 SSE 保活与三态协议分流

日期：2026-11-20
关联：`internal/server/api/prestream_keepalive.go`、`internal/server/api/nonstream_keepalive.go`、`internal/server/api/chat.go`、`internal/server/api/gemini.go`、`internal/server/api/openai.go`、`internal/server/api/responses_stream.go`

## 背景

AxonHub 部署在 Cloudflare 等具备空闲首字节超时限制（HTTP 524，100 秒无响应字节即断连）的反向代理/CDN 后时：
- 非流式请求已有 `nonStreamKeepAlive` 在 `Orchestrator.Process` 阻塞期间定期写 `"\n"` 保活；
- 但流式请求的 SSE 心跳原先只在 `Orchestrator.Process` 返回后进入 `writeSSEStream` 时才启动。由于默认开启重试预算时 `llm/pipeline/stream.go` 的 `preReadLlmStream` 会一直阻塞到上游产出首个有效内容事件（或全部同渠道/跨渠道重试耗尽），长上下文预填充或慢思考推理模型一旦首包等待超过 100 秒，下游就会被 Cloudflare 掐断返回 HTTP 524，网关侧表现为 `failed to stream request: context canceled`。
- 同时，原有的二元 `isNonStreamingRequest` 仅凭路径不含 `streamGenerateContent` 与 JSON body 无 `"stream": true` 判定非流式，存在对 `/v1/audio/speech` 二进制音频误写 `"\n"` + `application/json` 头、对 `/v1/audio/transcriptions` multipart 表单无法识别 `stream=true`、以及 Google/Gemini 原生端点（`GeminiHandlers`）未接入保活配置的问题。

## 方案

### 1. 三态预响应保活分流（`classifyPreProcessKeepAlive`）
将 `Orchestrator.Process` 阻塞期间的保活方式分为三态：
- `preProcessKeepAliveNone`：不写任何前置字节（适用于 `/v1/audio/speech` 二进制音频流、`/v1/audio/transcriptions` 的 `text/srt/vtt` 文本响应、Gemini 默认 JSON 数组流 `:streamGenerateContent`、或自定义非默认 `StreamWriter` 路径）。
- `preProcessKeepAliveNonStreamJSON`：定期写 `"\n"` 并锁定 `Content-Type: application/json`（适用于普通 `stream:false` JSON 请求、`/v1/responses/compact` 强制单 JSON 响应、Gemini `:generateContent` 非流式响应、以及 `response_format` 为 `""/json/verbose_json` 的非流式音频转写/翻译）。
- `preProcessKeepAliveSSE`：复用 `sse_keep_alive`（WebUI `streaming`）配置，在首个 `Interval` 到期后提交 SSE 响应头（`200 OK`、`Content-Type: text/event-stream`、`Cache-Control: no-cache`、`Connection: keep-alive`、`Access-Control-Allow-Origin: *`）并发送协议匹配的心跳帧：
  - OpenAI Chat / Completions / Responses、Audio Speech `stream_format:"sse"`、Audio Multipart `stream=true`、Gemini `:streamGenerateContent?alt=sse`：发送 `: keep-alive\n\n`；
  - Anthropic Messages：发送 `event: ping\ndata: {"type":"ping"}\n\n`。

Multipart 扫描铁律：遇到 `part.FileName() != ""` 或 `part.FormName() == "file"` 直接跳过（由 `NextPart()` 流式跳到下一个 boundary），仅用 `io.LimitReader(part, 64)` 读取 `stream` 和 `response_format` 两个小文本字段，避免大音频上传在 API 层产生额外内存拷贝。

### 2. 快路径零侵入与平滑交接
- **快路径零侵入**：首个 tick（`Interval`）到来前不写任何响应头或字节。常规快请求或快报错（如秒级 400/429/500）在首 tick 前返回时，HTTP 状态码、响应头与跨渠道重试行为与改动前字节级一致。
- **同步停止与交接**：`Process` 一旦返回，立即调用 `heartbeat.stop()` 并阻塞等待 `<-doneCh` 确保后台 ticker 协程完全退出后再由主协程操作 `c.Writer`。若首包前心跳已触发（`heartbeat.started() == true`），跳过已失效的 `writeForwardResponseHeaders`、重复 CORS 头设置以及 `primeCodexTurnStateHeader` 阻塞预读，直接进入 `writeSSEStream` 继续输出正文事件与流中空闲心跳。
- **Fail-Fast 客户端断连止损**：当保活启用时为 `Process` 派生可取消上下文；若保活协程写心跳报错（客户端已断开），立即调用 `cancelProcess()` 终止上游等待与重试。

### 3. 首包前已提交后的 SSE 错误收尾与脱敏边界
- 若首包前 SSE 心跳已刷出 `200 OK` 与 `text/event-stream` 头，而 `Process` 最终因所有重试耗尽返回 `err != nil`，通过 `writePreStreamSSEError` 输出对应协议的 SSE `event: error` 事件，并记录带 `upstream_status_code` 的 `WARN` 日志。
- **脱敏边界**：仅当错误确实来自上游（`pipeline.IsUpstreamError(err)` 为真，或经 `orchestrator.IsUpstreamTransportError(err)` 分类包装）时才应用 `UpstreamErrorPolicy`（隐藏或自定义文案）；网关本地错误（如无可用渠道、模型被拒）保持原始错误消息直出，不被伪装成上游错误。

## 被放弃方案及原因

- **新增独立的“首包前流式保活”配置开关**：前端与配置文案原本就定义为“流式请求等待期间定期发送 SSE 心跳注释，防止代理/CDN 空闲断连”，且首 tick 前零字节侵入已保证快请求与快报错不降级 HTTP 状态码；新增开关只会增加用户配置心智负担。
- **全生命周期单协程 Writer 状态机（替代两段式交接）**：`Process` 返回后主协程需根据 `started()` 决定是否执行 `writeForwardResponseHeaders` 和 `primeCodexTurnStateHeader` 读写 `c.Writer.Header()`；两段式在 `Process` 返回点同步 `stop()` 等待 `<-doneCh` 退出，所有权转移边界最清晰且无 `http.Header` 并发读写风险。
- **在 `preStreamErrorEncoder` 中无条件对所有 `Process` 错误调用 `pipeline.WrapUpstreamError`**：这会让 `applyUpstreamErrorPolicy` 把网关本地路由/校验报错也当成上游错误脱敏为 `"Upstream request failed"`，掩盖真实的网关配置问题。
