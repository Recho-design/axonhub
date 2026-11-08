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

// nonStreamKeepAlive writes whitespace heartbeats to the downstream response while the
// upstream call blocks, so intermediaries with an idle timeout (e.g. Cloudflare's
// 100-second limit) keep the connection open. If the upstream answers before the first
// tick, no byte is ever written and the response is byte-identical to a plain response.
type nonStreamKeepAlive struct {
	stopOnce   sync.Once
	stopCh     chan struct{}
	doneCh     chan struct{}
	wroteBytes atomic.Bool
}

// startNonStreamKeepAlive starts a heartbeat goroutine for a non-streaming request and
// returns its handle. The handle is always usable; a disabled or misconfigured feature
// yields a no-op handle so callers need no nil checks.
func startNonStreamKeepAlive(c *gin.Context, cfg NonStreamKeepAliveConfig) *nonStreamKeepAlive {
	heartbeat := &nonStreamKeepAlive{}
	if !cfg.Enabled || cfg.Interval <= 0 {
		return heartbeat
	}

	ctx := c.Request.Context()
	heartbeat.stopCh = make(chan struct{})
	heartbeat.doneCh = make(chan struct{})

	go func() {
		defer close(heartbeat.doneCh)

		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.stopCh:
				return
			case <-ticker.C:
				if err := heartbeat.writeHeartbeat(ctx, c.Writer); err != nil {
					log.Warn(ctx, "Failed to write non-stream keep-alive heartbeat", log.Cause(err))

					return
				}
			}
		}
	}()

	return heartbeat
}

// writeHeartbeat writes one heartbeat byte and flushes it downstream. The first write
// commits the status line, which is why the caller must consult started() afterwards.
func (h *nonStreamKeepAlive) writeHeartbeat(ctx context.Context, writer http.ResponseWriter) error {
	if !h.wroteBytes.Load() {
		// The body is a JSON document; set the content type before the first flush
		// locks the headers in.
		writer.Header().Set("Content-Type", "application/json")
	}

	setSSEWriteDeadline(ctx, writer, time.Now().Add(nonStreamKeepAliveWriteTimeout))
	defer clearSSEWriteDeadline(ctx, writer)

	if _, err := io.WriteString(writer, nonStreamKeepAlivePayload); err != nil {
		return err
	}

	// From here on the status line is on the wire and cannot be changed.
	h.wroteBytes.Store(true)

	return flushResponseWriter(writer)
}

// stop stops the heartbeat goroutine and waits until it exited. It is idempotent, so a
// deferred stop next to an explicit one is safe.
func (h *nonStreamKeepAlive) stop() {
	h.stopOnce.Do(func() {
		if h.stopCh != nil {
			close(h.stopCh)
		}
	})

	if h.doneCh != nil {
		<-h.doneCh
	}
}

// started reports whether heartbeat bytes already reached the client, meaning the
// status code and headers are committed and the body can only be completed, not restarted.
func (h *nonStreamKeepAlive) started() bool {
	return h.wroteBytes.Load()
}

// isNonStreamingRequest reports whether the request is expected to answer with a single
// JSON body. The check is conservative on purpose: any doubt disables the heartbeat,
// because prefixed bytes would corrupt a non-JSON or SSE response.
func isNonStreamingRequest(request *httpclient.Request) bool {
	if request == nil {
		return false
	}

	if strings.Contains(request.Path, geminiStreamPathFragment) {
		return false
	}

	if stream := gjson.GetBytes(request.Body, "stream"); stream.Exists() && stream.Type == gjson.True {
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
