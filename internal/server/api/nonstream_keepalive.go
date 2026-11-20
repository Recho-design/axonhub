// 职责: 提供非流式响应的空白心跳，以及两类预响应保活共用的 ticker 生命周期。
// 关联文件:
//
//	internal/server/api/chat.go
//	internal/server/api/prestream_keepalive.go
//	internal/server/api/sse_writer.go
//
// 遵循的 Rules:
//
//	.agents/rules/go-general.md
package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/llm/httpclient"
)

// NonStreamKeepAliveConfig controls downstream heartbeats for non-streaming requests.
type NonStreamKeepAliveConfig struct {
	Enabled  bool
	Interval time.Duration
}

const (
	// nonStreamKeepAlivePayload is leading whitespace for a JSON body, so clients that
	// unmarshal the final response are unaffected while byte-based proxies see traffic.
	nonStreamKeepAlivePayload = "\n"

	// nonStreamKeepAliveWriteTimeout bounds a single heartbeat write; without it a
	// stalled peer could hold the heartbeat goroutine past stop().
	nonStreamKeepAliveWriteTimeout = 10 * time.Second

	// geminiStreamPathFragment marks the Gemini streaming endpoint, whose body is not
	// a single JSON document and must never be prefixed with heartbeat bytes.
	geminiStreamPathFragment = "streamGenerateContent"
)

// keepAliveTicker is the lifecycle shared by every pre-process heartbeat: one ticker
// goroutine, an idempotent stop that waits for it to exit, and the flag that tells the
// handler whether the response is already committed. A zero value is a usable no-op.
type keepAliveTicker struct {
	stopOnce    sync.Once
	stopCh      chan struct{}
	doneCh      chan struct{}
	startedFlag atomic.Bool
}

// start launches the heartbeat loop. tick writes one heartbeat and returns its write
// error; the loop stops on the first failure. count starts at 1 for the first tick.
func (t *keepAliveTicker) start(
	ctx context.Context,
	interval time.Duration,
	panicMessage string,
	tick func(ctx context.Context, count int) error,
) {
	t.stopCh = make(chan struct{})
	t.doneCh = make(chan struct{})

	go t.run(ctx, interval, panicMessage, tick)
}

// run is the goroutine body. It recovers a panic so a keep-alive bug cannot crash the
// process, and closes doneCh so stop() can wait until the writer is free.
func (t *keepAliveTicker) run(
	ctx context.Context,
	interval time.Duration,
	panicMessage string,
	tick func(ctx context.Context, count int) error,
) {
	defer close(t.doneCh)
	defer func() {
		if r := recover(); r != nil {
			log.Error(ctx, panicMessage, log.Any("panic", r))
		}
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for count := 1; ; count++ {
		select {
		case <-ctx.Done():
			return
		case <-t.stopCh:
			return
		case <-ticker.C:
			if tick(ctx, count) != nil {
				return
			}
		}
	}
}

// stop stops the heartbeat goroutine and waits for it to exit so the caller can touch
// the response writer without racing the heartbeat. It is idempotent.
func (t *keepAliveTicker) stop() {
	t.stopOnce.Do(func() {
		if t.stopCh != nil {
			close(t.stopCh)
		}
	})

	if t.doneCh != nil {
		<-t.doneCh
	}
}

// started reports whether heartbeat bytes already reached the client, meaning the
// status code and headers are committed and the body can only be completed, not restarted.
func (t *keepAliveTicker) started() bool {
	return t.startedFlag.Load()
}

// nonStreamKeepAlive writes whitespace heartbeats to the downstream response while the
// upstream call blocks, so intermediaries with an idle timeout (e.g. Cloudflare's
// 100-second limit) keep the connection open. If the upstream answers before the first
// tick, no byte is ever written and the response is byte-identical to a plain response.
type nonStreamKeepAlive struct {
	keepAliveTicker
	writer       http.ResponseWriter
	onWriteError func()
}

// startNonStreamKeepAlive starts a heartbeat goroutine for a non-streaming request and
// returns its handle. The handle is always usable; a disabled or misconfigured feature
// yields a no-op handle so callers need no nil checks. onWriteError runs after a failed
// heartbeat so the caller can abort an upstream call whose client has gone away.
func startNonStreamKeepAlive(c *gin.Context, cfg NonStreamKeepAliveConfig, onWriteError func()) *nonStreamKeepAlive {
	heartbeat := &nonStreamKeepAlive{writer: c.Writer, onWriteError: onWriteError}
	if !cfg.Enabled || cfg.Interval <= 0 {
		return heartbeat
	}

	heartbeat.start(c.Request.Context(), cfg.Interval, "Panic in non-stream keep-alive", heartbeat.tick)

	return heartbeat
}

// tick writes one heartbeat. A failed write aborts the upstream call through
// onWriteError, because the client that vanished will never read the response.
func (h *nonStreamKeepAlive) tick(ctx context.Context, _ int) error {
	if err := h.writeHeartbeat(ctx); err != nil {
		log.Warn(ctx, "Failed to write non-stream keep-alive heartbeat", log.Cause(err))
		if h.onWriteError != nil {
			h.onWriteError()
		}

		return err
	}

	return nil
}

// writeHeartbeat writes one heartbeat byte and flushes it downstream. The first write
// commits the status line, which is why the caller must consult started() afterwards.
func (h *nonStreamKeepAlive) writeHeartbeat(ctx context.Context) error {
	if !h.startedFlag.Load() {
		// The body is a JSON document; set the content type before the first flush
		// locks the headers in.
		h.writer.Header().Set("Content-Type", "application/json")
	}

	setSSEWriteDeadline(ctx, h.writer, time.Now().Add(nonStreamKeepAliveWriteTimeout))
	defer clearSSEWriteDeadline(ctx, h.writer)

	if _, err := io.WriteString(h.writer, nonStreamKeepAlivePayload); err != nil {
		return err
	}

	// From here on the status line is on the wire and cannot be changed.
	h.startedFlag.Store(true)

	return flushResponseWriter(h.writer)
}

// isNonStreamingRequest reports whether the request is expected to answer with a single
// JSON body. It is kept for callers that only need the binary distinction; the handler
// itself uses the three-state classifyPreProcessKeepAlive, which also knows about audio,
// compact and multipart endpoints.
func isNonStreamingRequest(request *httpclient.Request) bool {
	if request == nil {
		return false
	}

	if strings.Contains(request.Path, geminiStreamPathFragment) {
		return false
	}

	if stream := gjson.GetBytes(request.Body, "stream"); stream.Type == gjson.True {
		return false
	}

	return true
}

// writeCommittedBody writes body into a response whose headers were already sent by the
// keep-alive heartbeat and flushes it, so the client sees the payload immediately.
func writeCommittedBody(writer gin.ResponseWriter, body []byte) error {
	if _, err := writer.Write(body); err != nil {
		return err
	}

	return flushResponseWriter(writer)
}
