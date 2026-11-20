// 职责: 按请求形态三态分流保活方式，并在上游首包到达前发送 SSE 心跳。
// 关联文件:
//
//	internal/server/api/chat.go
//	internal/server/api/nonstream_keepalive.go
//	internal/server/api/sse_writer.go
//	internal/server/api/openai.go
//
// 遵循的 Rules:
//
//	.agents/rules/go-general.md
//
// 注意事项:
//
//	首包前 SSE 心跳只在第一个 Interval 到期后写响应头，快路径必须与改动前字节级一致。
//	分类绝不能向二进制音频或非 JSON 文本响应写入 JSON 心跳字节。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

// multipartKeepAliveFieldLimit bounds a single non-file multipart field read; the
// fields used for classification (stream, response_format) are tiny.
const multipartKeepAliveFieldLimit = 64

// preProcessKeepAliveMode selects what may be written downstream while the
// orchestrator is still waiting for the upstream response.
type preProcessKeepAliveMode uint8

const (
	// preProcessKeepAliveNone writes nothing before the first upstream byte.
	preProcessKeepAliveNone preProcessKeepAliveMode = iota
	// preProcessKeepAliveNonStreamJSON writes "\n" heartbeats for a single JSON body.
	preProcessKeepAliveNonStreamJSON
	// preProcessKeepAliveSSE writes protocol-matched SSE heartbeats.
	preProcessKeepAliveSSE
)

// classifyPreProcessKeepAlive decides which pre-response heartbeat, if any, is safe
// for the request. Any doubt resolves to preProcessKeepAliveNone because a stray byte
// would corrupt binary or non-SSE responses.
func (handlers *ChatCompletionHandlers) classifyPreProcessKeepAlive(req *httpclient.Request) preProcessKeepAliveMode {
	if req == nil || len(req.Body) == 0 {
		return preProcessKeepAliveNone
	}

	path := req.Path
	if strings.Contains(path, geminiStreamPathFragment) {
		// Gemini streams are JSON arrays written by WriteGeminiStream unless the client
		// asked for alt=sse, in which case the default SSE writer owns the response and
		// protocol-matched SSE heartbeats are safe. Any other byte would corrupt the array.
		if geminiStreamSSE(req, handlers) {
			return preProcessKeepAliveSSE
		}

		return preProcessKeepAliveNone
	}
	if strings.HasSuffix(path, "/responses/compact") {
		return classifyCompactKeepAlive(handlers)
	}
	if strings.HasSuffix(path, "/audio/speech") {
		return classifySpeechKeepAlive(handlers, req)
	}
	if strings.HasSuffix(path, "/audio/transcriptions") || strings.HasSuffix(path, "/audio/translations") {
		return classifyMultipartKeepAlive(handlers, req)
	}

	if stream := gjson.GetBytes(req.Body, "stream"); stream.Type == gjson.True {
		if handlers.sseHeartbeatSupported() {
			return preProcessKeepAliveSSE
		}

		return preProcessKeepAliveNone
	}

	return preProcessKeepAliveNonStreamJSON
}

// geminiStreamSSE reports whether a Gemini stream request really answers SSE: the client
// asked for alt=sse and the handler kept the default SSE writer for it. Both must hold,
// because the writer decides the wire format while alt=sse is what selected it.
func geminiStreamSSE(req *httpclient.Request, handlers *ChatCompletionHandlers) bool {
	if !handlers.sseHeartbeatSupported() {
		return false
	}

	return strings.EqualFold(strings.TrimSpace(req.Query.Get("alt")), "sse")
}

// sseHeartbeatSupported reports whether the default SSE writer owns the response and
// the protocol has a heartbeat frame.
func (handlers *ChatCompletionHandlers) sseHeartbeatSupported() bool {
	return handlers.StreamWriter == nil && handlers.sseHeartbeatFormat != sseHeartbeatNone
}

// classifyCompactKeepAlive treats compact as a single JSON response even when the
// request body carries "stream": true.
func classifyCompactKeepAlive(handlers *ChatCompletionHandlers) preProcessKeepAliveMode {
	if handlers.StreamWriter != nil {
		return preProcessKeepAliveNone
	}

	return preProcessKeepAliveNonStreamJSON
}

// classifySpeechKeepAlive only allows SSE heartbeats for stream_format=sse; every
// other format returns binary audio.
func classifySpeechKeepAlive(handlers *ChatCompletionHandlers, req *httpclient.Request) preProcessKeepAliveMode {
	var body speechRouteRequestBody
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return preProcessKeepAliveNone
	}

	if strings.ToLower(strings.TrimSpace(body.StreamFormat)) != "sse" {
		return preProcessKeepAliveNone
	}
	if handlers.sseHeartbeatSupported() {
		return preProcessKeepAliveSSE
	}

	return preProcessKeepAliveNone
}

// classifyMultipartKeepAlive scans the small text fields of an audio upload form.
func classifyMultipartKeepAlive(handlers *ChatCompletionHandlers, req *httpclient.Request) preProcessKeepAliveMode {
	contentType := req.Headers.Get("Content-Type")
	if contentType == "" {
		contentType = req.ContentType
	}

	stream, responseFormat, ok := scanMultipartKeepAliveFields(req.Body, contentType)
	if !ok {
		return preProcessKeepAliveNone
	}
	if stream {
		if handlers.sseHeartbeatSupported() {
			return preProcessKeepAliveSSE
		}

		return preProcessKeepAliveNone
	}

	switch strings.ToLower(strings.TrimSpace(responseFormat)) {
	case "", "json", "verbose_json":
		return preProcessKeepAliveNonStreamJSON
	default:
		return preProcessKeepAliveNone
	}
}

// scanMultipartKeepAliveFields reads only the stream and response_format parts.
// File parts are skipped without reading: NextPart streams straight to the next
// boundary, so a large audio upload never lands in memory here.
func scanMultipartKeepAliveFields(body []byte, contentType string) (bool, string, bool) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return false, "", false
	}

	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	stream := false
	responseFormat := ""
	parts := 0

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			// A form without any part is not a valid audio upload; stay conservative.
			return stream, responseFormat, parts > 0
		}
		if err != nil {
			return false, "", false
		}
		parts++

		name := part.FormName()
		if part.FileName() != "" || name == "file" {
			continue
		}

		value, err := readMultipartTextField(part)
		if err != nil {
			return false, "", false
		}

		switch name {
		case "stream":
			stream = isTruthyMultipartStream(value)
		case "response_format":
			responseFormat = value
		}
	}
}

func readMultipartTextField(part io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(part, multipartKeepAliveFieldLimit))
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(data)), nil
}

func isTruthyMultipartStream(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// preStreamKeepAlive keeps an SSE response alive while the orchestrator waits for
// the upstream's first byte. Before the first tick it writes nothing at all.
type preStreamKeepAlive struct {
	keepAliveTicker
	writer       http.ResponseWriter
	format       sseHeartbeatFormat
	interval     time.Duration
	onWriteError func()
}

// startPreStreamKeepAlive starts the pre-first-byte SSE heartbeat. The returned
// handle is always usable; a disabled feature yields a no-op handle.
func startPreStreamKeepAlive(
	c *gin.Context,
	cfg SSEKeepAliveConfig,
	format sseHeartbeatFormat,
	onWriteError func(),
) *preStreamKeepAlive {
	heartbeat := &preStreamKeepAlive{
		writer:       c.Writer,
		format:       format,
		interval:     cfg.Interval,
		onWriteError: onWriteError,
	}
	if !cfg.Enabled || cfg.Interval <= 0 || format == sseHeartbeatNone {
		return heartbeat
	}

	heartbeat.start(c.Request.Context(), cfg.Interval, "Panic in pre-stream SSE keep-alive", heartbeat.tick)

	return heartbeat
}

// tick sends one heartbeat. A failed write aborts the upstream through onWriteError
// because the client is gone; otherwise the frame is logged.
func (h *preStreamKeepAlive) tick(ctx context.Context, count int) error {
	if err := h.sendHeartbeat(ctx); err != nil {
		log.Warn(ctx, "Failed to write pre-stream SSE heartbeat", log.Cause(err),
			log.Int("heartbeat_count", count))
		if h.onWriteError != nil {
			h.onWriteError()
		}

		return err
	}

	log.Info(ctx, "Pre-stream SSE heartbeat sent",
		log.Int("heartbeat_count", count),
		log.String("heartbeat_format", sseHeartbeatFormatName(h.format)),
		log.Duration("interval", h.interval),
	)

	return nil
}

// sendHeartbeat commits the SSE headers on the first tick, then writes a heartbeat
// frame. started is recorded before the first write because the status line is
// locked as soon as the flush reaches the wire.
func (h *preStreamKeepAlive) sendHeartbeat(ctx context.Context) error {
	if !h.startedFlag.Load() {
		setSSEResponseHeaders(h.writer.Header())
		h.writer.Header().Set("Access-Control-Allow-Origin", "*")
		h.startedFlag.Store(true)
	}

	return writeSSEHeartbeatEvent(ctx, h.writer, h.format)
}

// preProcessHeartbeat is the lifecycle the handler needs from a running keep-alive,
// so it never has to branch on the concrete mode.
type preProcessHeartbeat interface {
	stop()
	started() bool
}

// requestKeepAlive is the single pre-process heartbeat handle used by the handler.
// isSSE records that the heartbeat committed a text/event-stream response.
type requestKeepAlive struct {
	heartbeat preProcessHeartbeat
	isSSE     bool
}

// startPreProcessKeepAlive picks the handle for mode. A mode that must stay silent
// still gets a usable no-op handle.
func startPreProcessKeepAlive(
	c *gin.Context,
	mode preProcessKeepAliveMode,
	nonStreamCfg NonStreamKeepAliveConfig,
	sseCfg SSEKeepAliveConfig,
	format sseHeartbeatFormat,
	onWriteError func(),
) *requestKeepAlive {
	switch mode {
	case preProcessKeepAliveNonStreamJSON:
		return &requestKeepAlive{heartbeat: startNonStreamKeepAlive(c, nonStreamCfg, onWriteError)}
	case preProcessKeepAliveSSE:
		return &requestKeepAlive{
			heartbeat: startPreStreamKeepAlive(c, sseCfg, format, onWriteError),
			isSSE:     true,
		}
	default:
		return &requestKeepAlive{heartbeat: &keepAliveTicker{}}
	}
}

// keepAliveWillTick reports whether the selected mode will actually emit bytes, which
// is when the request context must carry a cancel to stop the upstream on write errors.
func keepAliveWillTick(
	mode preProcessKeepAliveMode,
	nonStreamCfg NonStreamKeepAliveConfig,
	sseCfg SSEKeepAliveConfig,
	format sseHeartbeatFormat,
) bool {
	switch mode {
	case preProcessKeepAliveNonStreamJSON:
		return nonStreamCfg.Enabled && nonStreamCfg.Interval > 0
	case preProcessKeepAliveSSE:
		return sseCfg.Enabled && sseCfg.Interval > 0 && format != sseHeartbeatNone
	default:
		return false
	}
}

func (h *requestKeepAlive) started() bool {
	return h.heartbeat.started()
}

func (h *requestKeepAlive) sseStarted() bool {
	return h.isSSE && h.heartbeat.started()
}

func (h *requestKeepAlive) stop() {
	h.heartbeat.stop()
}

// writePreStreamSSEError reports a Process failure as an SSE error event, because the
// pre-stream heartbeat already committed a 200 text/event-stream response.
func (handlers *ChatCompletionHandlers) writePreStreamSSEError(
	ctx context.Context,
	c *gin.Context,
	err error,
	httpErr *httpclient.Error,
) {
	log.Warn(ctx, "Pre-stream SSE keep-alive committed the response before the upstream error, sending SSE error event",
		log.Int("upstream_status_code", httpErr.StatusCode))

	var systemService *biz.SystemService
	if handlers.ChatCompletionOrchestrator != nil {
		systemService = handlers.ChatCompletionOrchestrator.SystemService
	}

	encodeErr, preparedErr := handlers.preStreamErrorEncoder(ctx, systemService, err)
	if writeErr := writeSSEErrorEvent(ctx, c.Writer, encodeErr, preparedErr); writeErr != nil {
		log.Warn(ctx, "Failed to write pre-stream SSE error event", log.Cause(writeErr))
	}
}

// preStreamErrorEncoder builds the protocol-specific error encoder and prepares err
// so the upstream error policy (hidden/custom message) is applied exactly once.
func (handlers *ChatCompletionHandlers) preStreamErrorEncoder(
	ctx context.Context,
	systemService *biz.SystemService,
	err error,
) (StreamErrorEncoder, error) {
	if handlers.streamAdapterFactory != nil {
		return handlers.responsesPreStreamErrorEncoder(ctx, systemService, err)
	}

	preparedErr := wrapQuotaExhaustedAsResponseError(err)
	if !errors.Is(preparedErr, context.Canceled) && !errors.Is(preparedErr, context.DeadlineExceeded) {
		// Classify transport-level interruptions before the policy runs so the stable
		// code and 502 semantics survive a hidden/custom message rewrite. A true
		// upstream HTTP/LLM error already carries pipeline.WrapUpstreamError from the
		// stream layer, so only transport errors need the extra marker here. Wrapping
		// everything would let the policy disguise gateway-local errors as upstream.
		if orchestrator.IsUpstreamTransportError(preparedErr) {
			preparedErr = orchestrator.ClassifyUpstreamTransportError(pipeline.WrapUpstreamError(preparedErr))
		}
		preparedErr = applyUpstreamErrorPolicy(ctx, preparedErr, systemService)
	}

	return encodeStreamError(FormatStreamError), preparedErr
}

// responsesPreStreamErrorEncoder is the Responses variant: its adapter encoder applies
// the policy but not the quota wrapper, and redacts everything it is given.
func (handlers *ChatCompletionHandlers) responsesPreStreamErrorEncoder(
	ctx context.Context,
	systemService *biz.SystemService,
	err error,
) (StreamErrorEncoder, error) {
	_, encodeErr := handlers.streamAdapterFactory(
		ctx, streams.SliceStream[*httpclient.StreamEvent](nil), systemService)
	preparedErr := wrapQuotaExhaustedAsResponseError(err)
	if !pipeline.IsUpstreamError(preparedErr) && !orchestrator.IsUpstreamTransportError(preparedErr) {
		// A gateway-local failure before the first byte (no available channel,
		// rejected model, ...) is not an upstream error. The adapter's encoder
		// redacts anything it is given, so emit the Responses wire shape directly
		// and keep the real message.
		return func(ctx context.Context, err error) (*httpclient.StreamEvent, error) {
			return encodeResponsesStreamError(ctx, err, 0)
		}, preparedErr
	}

	return encodeErr, preparedErr
}
