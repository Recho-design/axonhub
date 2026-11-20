package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/server/safeguards"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
)

const (
	errTypeQuotaExhausted = "quota_exhausted"
	errCodeQuotaExhausted = "quota_exhausted"
)

// StreamWriter is a function type for writing stream events to the response.
type StreamWriter func(c *gin.Context, stream streams.Stream[*httpclient.StreamEvent])

// SSEKeepAliveConfig controls downstream heartbeats for SSE-compatible APIs.
type SSEKeepAliveConfig struct {
	Enabled  bool
	Interval time.Duration
}

type sseHeartbeatFormat uint8

const (
	sseHeartbeatNone sseHeartbeatFormat = iota
	sseHeartbeatOpenAI
	sseHeartbeatAnthropic
)

type ChatCompletionHandlers struct {
	ChatCompletionOrchestrator *orchestrator.ChatCompletionOrchestrator
	StreamWriter               StreamWriter
	sseKeepAlive               SSEKeepAliveConfig
	nonStreamKeepAlive         NonStreamKeepAliveConfig
	keepAliveConfigProvider    func(context.Context) (biz.KeepAliveSettings, error)
	sseHeartbeatFormat         sseHeartbeatFormat
	streamAdapterFactory       streamAdapterFactory
	safeguardsConfig           safeguards.Config
	safeguardsConfigProvider   func(context.Context) (safeguards.Config, error)
	reviewer                   safeguards.ClassifierFunc
	inputUsageCache            *inputUsageCache
}

func NewChatCompletionHandlers(orchestrator *orchestrator.ChatCompletionOrchestrator) *ChatCompletionHandlers {
	return &ChatCompletionHandlers{
		ChatCompletionOrchestrator: orchestrator,
		StreamWriter:               WriteSSEStream,
		inputUsageCache:            newInputUsageCache(),
	}
}

// WithStreamWriter returns a new ChatCompletionHandlers with the specified stream writer.
func (handlers *ChatCompletionHandlers) WithStreamWriter(writer StreamWriter) *ChatCompletionHandlers {
	copy := *handlers
	copy.StreamWriter = writer
	return &copy
}

func (handlers *ChatCompletionHandlers) ChatCompletion(c *gin.Context) {
	ctx := c.Request.Context()

	// Use ReadHTTPRequest to parse the request
	genericReq, err := httpclient.ReadHTTPRequest(c.Request)
	if err != nil {
		httpErr := handlers.ChatCompletionOrchestrator.Inbound.TransformError(ctx, err)
		c.JSON(httpErr.StatusCode, json.RawMessage(httpErr.Body))

		return
	}

	handlers.ChatCompletionWithRequest(c, genericReq)
}

func (handlers *ChatCompletionHandlers) ChatCompletionWithRequest(c *gin.Context, genericReq *httpclient.Request) {
	started := time.Now()
	ctx := c.Request.Context()

	genericReq, evaluator, ok := handlers.prepareChatRequest(c, genericReq)
	if !ok {
		return
	}

	control := handlers.startChatProcess(ctx, c, genericReq, evaluator)
	defer control.stop()

	result, err := handlers.ChatCompletionOrchestrator.Process(control.ctx, genericReq)
	control.heartbeat.stop()

	if err != nil {
		handlers.handleChatProcessError(ctx, c, err, control.heartbeat)
		return
	}

	if !control.heartbeat.started() {
		writeForwardResponseHeaders(c, genericReq, result)
	}

	if result.ChatCompletion != nil {
		handlers.respondChatCompletion(ctx, c, result.ChatCompletion, evaluator, control.heartbeat)
		return
	}

	if result.ChatCompletionStream != nil {
		handlers.respondChatStream(ctx, c, genericReq, result, evaluator, control, started)
	}
}

// prepareChatRequest applies the Claude Code safeguards policy and returns the sanitized
// request. ok is false when an error response was already written.
func (handlers *ChatCompletionHandlers) prepareChatRequest(
	c *gin.Context,
	genericReq *httpclient.Request,
) (*httpclient.Request, *safeguards.Evaluator, bool) {
	ctx := c.Request.Context()

	if genericReq == nil || len(genericReq.Body) == 0 {
		JSONError(c, http.StatusBadRequest, errors.New("Request body is empty"))
		return nil, nil, false
	}

	config := handlers.safeguardsConfig
	// Only Claude Code requests asking for server-side safeguards need the settings lookup.
	if handlers.safeguardsConfigProvider != nil && gjson.GetBytes(genericReq.Body, "safeguards.#(type==\"dangerous_tool_use\")").Exists() {
		settings, err := handlers.safeguardsConfigProvider(ctx)
		if err != nil {
			log.Error(ctx, "Unable to load Claude Code safeguards settings", log.Cause(err))
			JSONError(c, http.StatusInternalServerError, errors.New("Unable to load Claude Code safeguards settings"))
			return nil, nil, false
		}
		config = settings
	}

	evaluator, prepared, err := prepareSafeguardsRequest(genericReq, config, handlers.reviewer)
	if err != nil {
		JSONError(c, http.StatusBadRequest, err)
		return nil, nil, false
	}

	return prepared, evaluator, true
}

// chatProcessControl carries everything the Process call and its response writers need.
type chatProcessControl struct {
	ctx           context.Context
	cancelProcess context.CancelFunc
	abortUpstream context.CancelFunc
	heartbeat     *requestKeepAlive
	sseKeepAlive  SSEKeepAliveConfig
}

// stop stops the heartbeat before releasing the process context so the goroutine never
// touches the response writer after the handler resumes.
func (control chatProcessControl) stop() {
	control.heartbeat.stop()
	if control.cancelProcess != nil {
		control.cancelProcess()
	}
}

// startChatProcess picks the pre-process heartbeat and installs a cancelable context
// whenever a heartbeat can emit bytes or safeguards can abort the upstream.
func (handlers *ChatCompletionHandlers) startChatProcess(
	ctx context.Context,
	c *gin.Context,
	genericReq *httpclient.Request,
	evaluator *safeguards.Evaluator,
) chatProcessControl {
	nonStreamKeepAlive, sseKeepAlive := handlers.resolveKeepAlive(ctx)
	mode := handlers.classifyPreProcessKeepAlive(genericReq)

	control := chatProcessControl{ctx: ctx, sseKeepAlive: sseKeepAlive}
	if evaluator != nil || keepAliveWillTick(mode, nonStreamKeepAlive, sseKeepAlive, handlers.sseHeartbeatFormat) {
		control.ctx, control.cancelProcess = context.WithCancel(ctx)
	}
	if evaluator != nil {
		control.abortUpstream = control.cancelProcess
	}
	control.heartbeat = startPreProcessKeepAlive(
		c, mode, nonStreamKeepAlive, sseKeepAlive, handlers.sseHeartbeatFormat, control.cancelProcess)

	return control
}

func (handlers *ChatCompletionHandlers) handleChatProcessError(
	ctx context.Context,
	c *gin.Context,
	err error,
	heartbeat *requestKeepAlive,
) {
	log.Error(ctx, "Error processing chat completion", log.Cause(err))

	httpErr := transformOrchestratorError(ctx, err, handlers.ChatCompletionOrchestrator)
	switch {
	case heartbeat.sseStarted():
		// The pre-stream heartbeat already committed 200 text/event-stream, so the
		// failure can only travel as an SSE error event.
		handlers.writePreStreamSSEError(ctx, c, err, httpErr)
	case heartbeat.started():
		// The heartbeat already committed a 200 with a JSON content type, so the
		// status cannot change anymore; deliver the error body inside it.
		log.Warn(ctx, "Non-stream keep-alive committed the response before the upstream error, returning the error body with status 200",
			log.Int("upstream_status_code", httpErr.StatusCode))
		if writeErr := writeCommittedBody(c.Writer, httpErr.Body); writeErr != nil {
			log.Warn(ctx, "Failed to write error body after non-stream keep-alive heartbeat", log.Cause(writeErr))
		}
	default:
		c.JSON(httpErr.StatusCode, json.RawMessage(httpErr.Body))
	}
}

func (handlers *ChatCompletionHandlers) respondChatCompletion(
	ctx context.Context,
	c *gin.Context,
	resp *httpclient.Response,
	evaluator *safeguards.Evaluator,
	heartbeat *requestKeepAlive,
) {
	committed := heartbeat.started()
	if heartbeat.sseStarted() {
		log.Warn(ctx, "Pre-stream SSE keep-alive committed the response but the orchestrator returned a non-stream body")
	}

	contentType := "application/json"
	if !committed {
		copyCodexTurnStateHeader(c.Writer.Header(), resp.Headers)

		if ct := resp.Headers.Get("Content-Type"); ct != "" {
			contentType = ct
		}
	}

	body, err := evaluator.AnnotateResponse(ctx, resp.Body)
	if err != nil {
		JSONError(c, http.StatusBadGateway, err)
		return
	}

	if committed {
		writeCommittedChatCompletion(ctx, c, resp, body)
		return
	}

	c.Data(resp.StatusCode, contentType, body)
}

// writeCommittedChatCompletion writes the body of a response whose status line was
// already committed by the heartbeat, so the upstream status can no longer be forwarded.
func writeCommittedChatCompletion(ctx context.Context, c *gin.Context, resp *httpclient.Response, body []byte) {
	if resp.StatusCode != http.StatusOK {
		log.Debug(ctx, "Non-stream keep-alive committed status 200 for a non-200 upstream response",
			log.Int("upstream_status_code", resp.StatusCode))
	}
	if writeErr := writeCommittedBody(c.Writer, body); writeErr != nil {
		log.Warn(ctx, "Failed to write response body after non-stream keep-alive heartbeat", log.Cause(writeErr))
	}
}

func (handlers *ChatCompletionHandlers) respondChatStream(
	ctx context.Context,
	c *gin.Context,
	genericReq *httpclient.Request,
	result orchestrator.ChatCompletionResult,
	evaluator *safeguards.Evaluator,
	control chatProcessControl,
	started time.Time,
) {
	heartbeat := control.heartbeat
	committed := heartbeat.started()
	if committed && !heartbeat.sseStarted() {
		log.Warn(ctx, "Non-stream keep-alive committed the response but the orchestrator returned a stream")
	}

	defer closeChatStream(ctx, result.ChatCompletionStream)

	if !committed {
		// The pre-stream heartbeat already sent this header with the SSE headers;
		// adding it now would be a no-op after the response is committed.
		c.Header("Access-Control-Allow-Origin", "*")
	}

	source := wrapAnthropicInputUsage(result.ChatCompletionStream, genericReq,
		newInputUsageCalibration(ctx, handlers.inputUsageCache, result.Route, started))
	source = wrapSafeguardsStream(ctx, source, evaluator, control.abortUpstream)
	stream := newUpstreamErrorStream(ctx, source, handlers.ChatCompletionOrchestrator.SystemService)
	if !committed && strings.HasSuffix(genericReq.Path, "/responses") {
		// Header priming reads the first event; after the response is committed the
		// turn-state header can no longer be sent, so skip the extra read.
		stream = primeCodexTurnStateHeader(c.Writer.Header(), stream)
	}
	if handlers.StreamWriter != nil {
		handlers.StreamWriter(c, stream)
		return
	}

	handlers.writeSSEStream(c, stream, control.sseKeepAlive)
}

func closeChatStream(ctx context.Context, stream streams.Stream[*httpclient.StreamEvent]) {
	log.Debug(ctx, "Close chat stream")

	err := stream.Close()
	if waiter, ok := stream.(streams.FinalizationWaiter); ok {
		waiter.WaitForFinalization()
	}
	if err != nil {
		logger.Error(ctx, "Error closing stream", log.Cause(err))
	}
}

func writeForwardResponseHeaders(c *gin.Context, request *httpclient.Request, result orchestrator.ChatCompletionResult) {
	if !result.CodexResponseHeadersSupported || request == nil || !strings.HasSuffix(request.Path, "/responses") || codex.GetSessionIDFromHeaders(request.Headers) == "" {
		return
	}

	var headers http.Header
	if result.ChatCompletion != nil {
		headers = result.ChatCompletion.Headers
	} else {
		headers = httpclient.GetResponseHeaders(result.ChatCompletionStream)
	}

	_ = httpclient.MergeForwardResponseHeaders(c.Writer.Header(), headers)
}

func copyCodexTurnStateHeader(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}

	if value := src.Get(codex.TurnStateHeader); value != "" {
		dst.Set(codex.TurnStateHeader, value)
	}
}

// primedStream keeps the first upstream event available after response headers
// have been copied to the downstream writer. HTTP stream executors expose
// transport headers on that first event, and SSE headers must be sent before
// the first flush.
type primedStream struct {
	stream  streams.Stream[*httpclient.StreamEvent]
	first   *httpclient.StreamEvent
	pending bool
}

func primeCodexTurnStateHeader(dst http.Header, stream streams.Stream[*httpclient.StreamEvent]) streams.Stream[*httpclient.StreamEvent] {
	if stream == nil || !stream.Next() {
		return stream
	}

	first := stream.Current()
	if first != nil {
		copyCodexTurnStateHeader(dst, first.Headers)
	}

	return &primedStream{stream: stream, first: first, pending: true}
}

func (s *primedStream) Next() bool {
	if s.pending {
		return true
	}

	return s.stream.Next()
}

func (s *primedStream) Current() *httpclient.StreamEvent {
	if s.pending {
		s.pending = false
		return s.first
	}

	return s.stream.Current()
}

func (s *primedStream) Err() error {
	return s.stream.Err()
}

func (s *primedStream) ExpectedStreamChoices() int {
	return streamExpectedChoices(s.stream)
}

func (s *primedStream) Close() error {
	return s.stream.Close()
}

// StreamErrorFormatter formats a stream error into a JSON-serializable object for SSE error events.
type StreamErrorFormatter func(ctx context.Context, err error) any

// maxStreamEventsAfterCancel bounds how many events the stream writers drain after
// the request context is canceled. Draining lets persistence wrappers observe a
// buffered terminal event, but streams are expected to end promptly on cancellation;
// the cap only guards against implementations that ignore it.
const maxStreamEventsAfterCancel = 256

const sseWriteTimeout = 30 * time.Second

// WriteSSEStream writes stream events as Server-Sent Events (SSE) with default error formatting.
func WriteSSEStream(c *gin.Context, stream streams.Stream[*httpclient.StreamEvent]) {
	WriteSSEStreamWithErrorFormatter(c, stream, FormatStreamError)
}

// WriteSSEStreamWithErrorFormatter writes stream events as SSE with a custom error formatter.
func WriteSSEStreamWithErrorFormatter(c *gin.Context, stream streams.Stream[*httpclient.StreamEvent], formatErr StreamErrorFormatter) {
	writeSSEStream(c, stream, formatErr, SSEKeepAliveConfig{}, sseHeartbeatNone)
}

func writeSSEStream(
	c *gin.Context,
	stream streams.Stream[*httpclient.StreamEvent],
	formatErr StreamErrorFormatter,
	keepAlive SSEKeepAliveConfig,
	heartbeatFormat sseHeartbeatFormat,
) {
	writeEncodedSSEStream(c, stream, encodeStreamError(formatErr), keepAlive, heartbeatFormat)
}

func writeEncodedSSEStream(
	c *gin.Context,
	stream streams.Stream[*httpclient.StreamEvent],
	encodeErr StreamErrorEncoder,
	keepAlive SSEKeepAliveConfig,
	heartbeatFormat sseHeartbeatFormat,
) {
	if !keepAlive.Enabled || keepAlive.Interval <= 0 || heartbeatFormat == sseHeartbeatNone {
		writeSSEStreamWithoutHeartbeat(c, stream, encodeErr)
		return
	}

	writeEncodedSSEStreamWithHeartbeat(c, stream, encodeErr, keepAlive.Interval, heartbeatFormat)
}

func writeSSEStreamWithoutHeartbeat(c *gin.Context, stream streams.Stream[*httpclient.StreamEvent], formatErr StreamErrorEncoder) {
	ctx := c.Request.Context()
	clientDisconnected := false

	defer func() {
		clearSSEWriteDeadline(ctx, c.Writer)
		if clientDisconnected {
			log.Warn(ctx, "Client disconnected")
		}
	}()

	// Set SSE headers
	setSSEHeaders(c)
	if err := flushSSE(ctx, c.Writer); err != nil {
		clientDisconnected = true
		log.Warn(ctx, "Failed to flush SSE headers", log.Cause(err))

		return
	}

	// Do not pre-check ctx.Done() before Next(). If the client disconnects right
	// after receiving the terminal event, a preferential ctx.Done() check can abort
	// before Next() drains EOF / the last buffered chunk, causing Close() to mark the
	// request canceled even though the stream completed. This relies on the stream
	// contract that Next() returns false promptly once cancellation is observed and
	// its buffer is drained; eventsAfterCancel bounds streams that violate it.
	eventsAfterCancel := 0
	terminalSeen := false
	terminalTracker := orchestrator.NewStreamTerminalTracker(streamExpectedChoices(stream))

	for {
		if !stream.Next() {
			writeSSEStreamEnd(c, ctx, stream.Err(), formatErr, terminalSeen, &clientDisconnected)

			return
		}

		if ctx.Err() != nil {
			eventsAfterCancel++
			if eventsAfterCancel > maxStreamEventsAfterCancel {
				log.Warn(ctx, "Stream still producing after cancellation, aborting drain",
					log.Int("events_after_cancel", eventsAfterCancel))
				writeSSEStreamEnd(c, ctx, ctx.Err(), formatErr, terminalSeen, &clientDisconnected)

				return
			}
		}

		cur := stream.Current()
		if err := writeStreamDelivery(ctx, stream, func() (bool, error) {
			if err := writeSSEEvent(ctx, c.Writer, cur.Type, cur.Data); err != nil {
				return false, err
			}
			terminal := !terminalSeen && terminalTracker.Observe(cur)
			if terminal {
				terminalSeen = true
			}
			return terminal, nil
		}); err != nil {
			clientDisconnected = true
			log.Warn(ctx, "Failed to write SSE event", log.Cause(err))

			return
		}
		log.Debug(ctx, "write stream event", log.Any("event", cur))
	}
}

func streamExpectedChoices(stream streams.Stream[*httpclient.StreamEvent]) int {
	if s, ok := stream.(interface{ ExpectedStreamChoices() int }); ok {
		return s.ExpectedStreamChoices()
	}
	return 0
}

func writeSSEStreamWithHeartbeat(
	c *gin.Context,
	stream streams.Stream[*httpclient.StreamEvent],
	formatErr StreamErrorFormatter,
	interval time.Duration,
	heartbeatFormat sseHeartbeatFormat,
) {
	writeEncodedSSEStreamWithHeartbeat(c, stream, encodeStreamError(formatErr), interval, heartbeatFormat)
}

func writeEncodedSSEStreamWithHeartbeat(
	c *gin.Context,
	stream streams.Stream[*httpclient.StreamEvent],
	formatErr StreamErrorEncoder,
	interval time.Duration,
	heartbeatFormat sseHeartbeatFormat,
) {
	ctx := c.Request.Context()
	clientDisconnected := false

	defer func() {
		clearSSEWriteDeadline(ctx, c.Writer)
		if clientDisconnected {
			log.Warn(ctx, "Client disconnected")
		}
	}()

	setSSEHeaders(c)
	if err := flushSSE(ctx, c.Writer); err != nil {
		clientDisconnected = true
		log.Warn(ctx, "Failed to flush SSE headers", log.Cause(err))

		return
	}

	reader := newSSEStreamReader(ctx, stream)
	// The caller closes the stream after this function returns. Wait for the
	// reader first so Close cannot race with Next or Current.
	defer reader.Stop()

	timer := time.NewTimer(interval)
	defer timer.Stop()

	timerC := timer.C
	ctxDone := ctx.Done()
	eventsAfterCancel := 0
	terminalSeen := false
	terminalTracker := orchestrator.NewStreamTerminalTracker(streamExpectedChoices(stream))
	heartbeatCount := 0

	for {
		select {
		case <-ctxDone:
			if errors.Is(ctx.Err(), context.Canceled) {
				clientDisconnected = true
			}
			ctxDone = nil
			stopTimer(timer)
			timerC = nil

		case result := <-reader.Results():
			if result.done {
				writeSSEStreamEnd(c, ctx, result.err, formatErr, terminalSeen, &clientDisconnected)
				return
			}

			if ctx.Err() != nil {
				eventsAfterCancel++
				if eventsAfterCancel > maxStreamEventsAfterCancel {
					log.Warn(ctx, "Stream still producing after cancellation, aborting drain",
						log.Int("events_after_cancel", eventsAfterCancel))
					writeSSEStreamEnd(c, ctx, ctx.Err(), formatErr, terminalSeen, &clientDisconnected)
					return
				}
			}

			cur := result.event
			if err := writeStreamDelivery(ctx, stream, func() (bool, error) {
				if err := writeSSEEvent(ctx, c.Writer, cur.Type, cur.Data); err != nil {
					return false, err
				}
				terminal := !terminalSeen && terminalTracker.Observe(cur)
				if terminal {
					terminalSeen = true
				}
				return terminal, nil
			}); err != nil {
				clientDisconnected = true
				log.Warn(ctx, "Failed to write SSE event", log.Cause(err))

				return
			}
			log.Debug(ctx, "write stream event", log.Any("event", cur))

			if timerC != nil {
				resetTimer(timer, interval)
			}

		case <-timerC:
			if err := writeSSEHeartbeatEvent(ctx, c.Writer, heartbeatFormat); err != nil {
				clientDisconnected = true
				log.Warn(ctx, "Failed to write SSE heartbeat", log.Cause(err))
				return
			}

			heartbeatCount++
			log.Info(ctx, "SSE heartbeat sent",
				log.Int("heartbeat_count", heartbeatCount),
				log.String("heartbeat_format", sseHeartbeatFormatName(heartbeatFormat)),
				log.Duration("interval", interval),
			)

			timer.Reset(interval)
		}
	}
}

// writeSSEStreamEnd finalizes the SSE response once the stream is drained.
//
// terminalSeen reports whether a completion marker was actually written to the
// client. An upstream that ends at EOF without one produces no stream error (see
// the io.EOF branch in the SSE decoder), so without this check the response would
// end silently and the client would read a truncated generation as a successful
// completion. The orchestrator detects the same condition, but only in Close(),
// which runs after this writer returns and the body is already flushed — too late
// to tell the client anything.
func writeSSEStreamEnd(
	c *gin.Context,
	ctx context.Context,
	streamErr error,
	formatErr StreamErrorEncoder,
	terminalSeen bool,
	clientDisconnected *bool,
) {
	switch {
	case terminalSeen:
		if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
			log.Warn(ctx, "Stream error after terminal event was delivered, suppressing trailing error event", log.Cause(streamErr))
		}
	case errors.Is(ctx.Err(), context.DeadlineExceeded) &&
		(streamErr == nil || errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded)):
		streamErr = ctx.Err()
		log.Error(ctx, "Stream deadline exceeded", log.Cause(streamErr))
		if err := writeSSEErrorEvent(ctx, c.Writer, formatErr, streamErr); err != nil {
			*clientDisconnected = true
			log.Warn(ctx, "Failed to write SSE deadline error", log.Cause(err))
		}
	case streamErr != nil:
		if errors.Is(streamErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			*clientDisconnected = true

			if !errors.Is(streamErr, context.Canceled) {
				log.Warn(ctx, "Stream error after client disconnected", log.Cause(streamErr))
			}
		} else {
			log.Error(ctx, "Error in stream", log.Cause(streamErr))
			if err := writeSSEErrorEvent(ctx, c.Writer, formatErr, streamErr); err != nil {
				*clientDisconnected = true
				log.Warn(ctx, "Failed to write SSE stream error", log.Cause(err))
			}
		}
	case errors.Is(ctx.Err(), context.Canceled):
		*clientDisconnected = true
	case !terminalSeen:
		log.Error(ctx, "Stream ended without terminal event, reporting incomplete stream to client",
			log.Cause(orchestrator.ErrStreamIncomplete))
		if err := writeSSEErrorEvent(ctx, c.Writer, formatErr, orchestrator.ErrStreamIncomplete); err != nil {
			*clientDisconnected = true
			log.Warn(ctx, "Failed to write incomplete SSE stream error", log.Cause(err))
		}
	}
}

// WriteBinaryStream writes raw bytes from stream events directly to the response body.
// The first chunk type is treated as the stream Content-Type when present.
func WriteBinaryStream(c *gin.Context, stream streams.Stream[*httpclient.StreamEvent]) {
	ctx := c.Request.Context()
	clientDisconnected := false
	headersWritten := false
	contentType := "application/octet-stream"

	defer func() {
		if clientDisconnected {
			log.Warn(ctx, "Client disconnected")
		}
	}()

	// Same as WriteSSEStream: do not pre-check ctx.Done() before Next(), so a
	// disconnect right after the terminal chunk does not skip drain / completion.
	// The drain after cancellation is bounded by eventsAfterCancel.
	eventsAfterCancel := 0
	terminalDelivered := false

	for {
		if !stream.Next() {
			if err := stream.Err(); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
					clientDisconnected = true

					// Keep genuine upstream failures visible even when the client is gone.
					if !errors.Is(err, context.Canceled) {
						log.Warn(ctx, "Binary stream error after client disconnected", log.Cause(err))
					}
				} else {
					log.Error(ctx, "Error in binary stream", log.Cause(err))
					if !headersWritten {
						failure := orchestrator.ClassifyUpstreamTransportError(err)
						c.JSON(streamErrorStatus(failure), FormatStreamError(ctx, failure))

						return
					}
				}
			} else if errors.Is(ctx.Err(), context.Canceled) {
				clientDisconnected = true
			}

			if err := writeStreamDelivery(ctx, stream, func() (bool, error) {
				return terminalDelivered, flushResponseWriter(c.Writer)
			}); err != nil {
				clientDisconnected = true
				log.Warn(ctx, "Failed to flush binary stream", log.Cause(err))
			}

			return
		}

		if ctx.Err() != nil {
			eventsAfterCancel++
			if eventsAfterCancel > maxStreamEventsAfterCancel {
				clientDisconnected = true

				log.Warn(ctx, "Binary stream still producing after cancellation, aborting drain",
					log.Int("events_after_cancel", eventsAfterCancel))

				return
			}
		}

		cur := stream.Current()
		if cur != nil && cur.Type == httpclient.BinaryStreamDoneEventType {
			terminalDelivered = true
			continue
		}

		if cur == nil || len(cur.Data) == 0 {
			continue
		}

		if !headersWritten {
			if ct := strings.TrimSpace(cur.Type); ct != "" {
				contentType = ct
			}

			c.Header("Content-Type", contentType)
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("Access-Control-Allow-Origin", "*")
			headersWritten = true
		}

		if n, err := c.Writer.Write(cur.Data); err != nil {
			clientDisconnected = true
			log.Warn(ctx, "Failed to write binary stream chunk", log.Cause(err))

			return
		} else if n != len(cur.Data) {
			clientDisconnected = true
			log.Warn(ctx, "Failed to write binary stream chunk", log.Cause(io.ErrShortWrite))

			return
		}

		if err := flushResponseWriter(c.Writer); err != nil {
			clientDisconnected = true
			log.Warn(ctx, "Failed to flush binary stream", log.Cause(err))

			return
		}
	}
}

func streamErrorStatus(err error) int {
	var quotaErr *orchestrator.QuotaExhaustedError
	if errors.As(err, &quotaErr) {
		return http.StatusServiceUnavailable
	}

	var respErr *llm.ResponseError
	if errors.As(err, &respErr) && respErr.StatusCode != 0 {
		return respErr.StatusCode
	}

	var httpErr *httpclient.Error
	if errors.As(err, &httpErr) && httpErr.StatusCode != 0 {
		return httpErr.StatusCode
	}

	return http.StatusInternalServerError
}

// FormatStreamError formats a stream error into an OpenAI-compatible JSON error object.
// When the error carries no upstream request id, the gateway's own request id from ctx
// is used so clients can always correlate an error event with the request log.
func FormatStreamError(ctx context.Context, err error) any {
	errType := "server_error"
	errCode := ""
	requestID := ""

	var quotaErr *orchestrator.QuotaExhaustedError
	if errors.As(err, &quotaErr) {
		return gin.H{
			"error": gin.H{
				"message": quotaErr.Error(),
				"type":    errTypeQuotaExhausted,
				"code":    errCodeQuotaExhausted,
			},
		}
	}

	var respErr *llm.ResponseError
	if errors.As(err, &respErr) {
		if respErr.Detail.Type != "" {
			errType = respErr.Detail.Type
		}

		errCode = respErr.Detail.Code
		requestID = respErr.Detail.RequestID

		return gin.H{
			"error": gin.H{
				"message": respErr.Detail.Message,
				"type":    errType,
				"code":    errCode,
			},
			"request_id": streamErrorRequestID(ctx, requestID),
		}
	}

	var httpErr *httpclient.Error
	if errors.As(err, &httpErr) && len(httpErr.Body) > 0 {
		if t := gjson.GetBytes(httpErr.Body, "error.type"); t.Exists() && t.Type == gjson.String && t.String() != "" {
			errType = t.String()
		}

		if c := gjson.GetBytes(httpErr.Body, "error.code"); c.Exists() && c.Type == gjson.String && c.String() != "" {
			errCode = c.String()
		}

		if rid := gjson.GetBytes(httpErr.Body, "request_id"); rid.Exists() && rid.Type == gjson.String && rid.String() != "" {
			requestID = rid.String()
		}
	}

	return gin.H{
		"error": gin.H{
			"message": orchestrator.ExtractErrorMessage(err),
			"type":    errType,
			"code":    errCode,
		},
		"request_id": streamErrorRequestID(ctx, requestID),
	}
}

// streamErrorRequestID returns the upstream request id when present, otherwise the
// gateway request id stored in ctx (the value echoed in the AH-Request-Id header).
func streamErrorRequestID(ctx context.Context, requestID string) string {
	if requestID != "" || ctx == nil {
		return requestID
	}

	if id, ok := contexts.GetRequestID(ctx); ok {
		return id
	}

	return ""
}

func wrapQuotaExhaustedAsResponseError(err error) error {
	if err == nil {
		return nil
	}

	var quotaErr *orchestrator.QuotaExhaustedError
	if errors.As(err, &quotaErr) {
		return &llm.ResponseError{
			StatusCode: http.StatusServiceUnavailable,
			Detail: llm.ErrorDetail{
				Message: quotaErr.Error(),
				Type:    errTypeQuotaExhausted,
				Code:    errCodeQuotaExhausted,
			},
		}
	}

	return err
}
