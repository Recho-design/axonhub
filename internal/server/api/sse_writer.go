// 职责: 提供带写超时与错误聚合的底层 SSE 写入、刷新与心跳帧能力。
// 关联文件:
//
//	internal/server/api/chat.go
//	internal/server/api/prestream_keepalive.go
//	internal/server/api/sse_encoder.go
//
// 遵循的 Rules:
//
//	.agents/rules/go-general.md
//
// 注意事项:
//
//	所有 SSE 写入都先刷新写超时再写，避免慢客户端长期占用写入协程。
package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-contrib/sse"
	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/orchestrator"
)

func writeSSEErrorEvent(ctx context.Context, writer http.ResponseWriter, encodeErr StreamErrorEncoder, err error) error {
	event, encodeError := encodeErr(ctx, orchestrator.ClassifyUpstreamTransportError(err))
	if encodeError != nil {
		return encodeError
	}
	return writeSSEEvent(ctx, writer, event.Type, event.Data)
}

func writeSSEEvent(ctx context.Context, writer http.ResponseWriter, event string, data any) error {
	return writeAndFlushSSE(ctx, writer, func(writer io.Writer) error {
		return sse.Encode(writer, sse.Event{Event: event, Data: data})
	})
}

type sseErrorWriter struct {
	writer io.Writer
	err    error
}

func (w *sseErrorWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}

	n, err := w.writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
	}

	return n, err
}

func (w *sseErrorWriter) WriteString(data string) (int, error) {
	if w.err != nil {
		return 0, w.err
	}

	n, err := io.WriteString(w.writer, data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
	}

	return n, err
}

func writeSSEHeartbeatEvent(ctx context.Context, writer http.ResponseWriter, format sseHeartbeatFormat) error {
	return writeAndFlushSSE(ctx, writer, func(writer io.Writer) error {
		return writeSSEHeartbeat(writer, format)
	})
}

func flushSSE(ctx context.Context, writer http.ResponseWriter) error {
	return writeAndFlushSSE(ctx, writer, nil)
}

func writeAndFlushSSE(ctx context.Context, writer http.ResponseWriter, write func(io.Writer) error) error {
	refreshSSEWriteDeadline(ctx, writer)
	defer clearSSEWriteDeadline(ctx, writer)

	if write != nil {
		errorWriter := &sseErrorWriter{writer: writer}
		if err := write(errorWriter); err != nil {
			return err
		}
		if errorWriter.err != nil {
			return errorWriter.err
		}
	}

	return flushResponseWriter(writer)
}

type responseWriterFlushError interface {
	FlushError() error
}

type responseWriterUnwrapper interface {
	Unwrap() http.ResponseWriter
}

func flushResponseWriter(writer http.ResponseWriter) error {
	if headerWriter, ok := writer.(interface{ WriteHeaderNow() }); ok {
		headerWriter.WriteHeaderNow()
	}
	if flusher, ok := writer.(responseWriterFlushError); ok {
		return flusher.FlushError()
	}
	if unwrapper, ok := writer.(responseWriterUnwrapper); ok {
		return flushResponseWriter(unwrapper.Unwrap())
	}
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()

		return nil
	}

	return http.ErrNotSupported
}

func refreshSSEWriteDeadline(ctx context.Context, writer http.ResponseWriter) {
	setSSEWriteDeadline(ctx, writer, time.Now().Add(sseWriteTimeout))
}

func clearSSEWriteDeadline(ctx context.Context, writer http.ResponseWriter) {
	setSSEWriteDeadline(ctx, writer, time.Time{})
}

func setSSEWriteDeadline(ctx context.Context, writer http.ResponseWriter, deadline time.Time) {
	err := http.NewResponseController(writer).SetWriteDeadline(deadline)
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Warn(ctx, "Failed to set SSE write deadline", log.Cause(err))
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetTimer(timer *time.Timer, interval time.Duration) {
	stopTimer(timer)
	timer.Reset(interval)
}

func setSSEHeaders(c *gin.Context) {
	setSSEResponseHeaders(c.Writer.Header())
}

func setSSEResponseHeaders(header http.Header) {
	header.Set("Content-Type", sse.ContentType)
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
}

func writeSSEHeartbeat(writer io.Writer, format sseHeartbeatFormat) error {
	switch format {
	case sseHeartbeatOpenAI:
		_, err := io.WriteString(writer, ": keep-alive\n\n")
		return err
	case sseHeartbeatAnthropic:
		_, err := io.WriteString(writer, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		return err
	default:
		return errors.New("unsupported SSE heartbeat format")
	}
}

func sseHeartbeatFormatName(format sseHeartbeatFormat) string {
	switch format {
	case sseHeartbeatOpenAI:
		return "openai"
	case sseHeartbeatAnthropic:
		return "anthropic"
	default:
		return "unknown"
	}
}
