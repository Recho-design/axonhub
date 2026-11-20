// 职责: 验证 Gemini 原生端点在 alt=sse、JSON 数组流与非流式三种形态下的保活行为。
// 关联文件:
//
//	internal/server/api/gemini.go
//	internal/server/api/prestream_keepalive.go
//	internal/server/api/prestream_keepalive_test.go
//
// 遵循的 Rules:
//
//	.agents/rules/go-general.md
//
// 注意事项:
//
//	Gemini 上游始终以 ?alt=sse 应答，客户端是否走 SSE 只由请求的 alt 决定。
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

const (
	preStreamGeminiModel = "gemini-test"
	preStreamGeminiBody  = `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`

	preStreamGeminiResponse = `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2},"modelVersion":"gemini-test","responseId":"resp-gemini-test"}`
)

// preStreamGeminiSSEChunks are the upstream Gemini SSE payloads. Both client forms
// share them because the gateway always requests ?alt=sse from a Gemini channel.
var preStreamGeminiSSEChunks = [...]string{
	`{"responseId":"resp-gemini-test","modelVersion":"gemini-test","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]}}]}`,
	`{"responseId":"resp-gemini-test","modelVersion":"gemini-test","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":" world"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"totalTokenCount":3}}`,
}

// keepAliveGeminiCase builds a Gemini streamGenerateContent row for classification.
// altSSE controls the alt query that GenerateContent uses to pick the SSE writer.
func keepAliveGeminiCase(
	name string,
	handlers *ChatCompletionHandlers,
	altSSE bool,
	want preProcessKeepAliveMode,
) preProcessKeepAliveCase {
	req := &httpclient.Request{
		Path: "/v1beta/models/gpt:streamGenerateContent",
		Body: []byte(`{"contents":[]}`),
	}
	if altSSE {
		req.Query = url.Values{"alt": []string{"sse"}}
	}

	return preProcessKeepAliveCase{name: name, handlers: handlers, req: req, want: want}
}

// runGemini posts one request through the real GenerateContent route and returns the
// full response. opts.inbound must be "gemini".
func (f *preStreamFixture) runGemini(
	t *testing.T,
	opts preStreamHandlerOptions,
	path string,
	body string,
) (*http.Response, string) {
	t.Helper()

	handlers := &GeminiHandlers{ChatCompletionHandlers: f.handler(t, opts)}
	engine := gin.New()
	engine.Use(f.testContext)
	engine.POST("/v1beta/models/*action", handlers.GenerateContent)
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, server.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp, string(data)
}

// preStreamGeminiSSEUpstream answers with Gemini SSE chunks after the delay. The mock
// ignores the request path because the outbound URL always carries ?alt=sse.
func preStreamGeminiSSEUpstream(delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		for _, chunk := range preStreamGeminiSSEChunks {
			//nolint:errcheck // the gateway owns the read side of this body
			_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func TestPreStreamKeepAliveGeminiEndToEnd(t *testing.T) {
	t.Run("streamGenerateContent alt=sse", func(t *testing.T) {
		fixture := newPreStreamFixture(t, llm.APIFormatGeminiContents, preStreamGeminiModel,
			preStreamGeminiSSEUpstream(300*time.Millisecond))

		resp, body := fixture.runGemini(t, preStreamHandlerOptions{
			inbound: "gemini",
			sse:     SSEKeepAliveConfig{Enabled: true, Interval: 20 * time.Millisecond},
		}, "/v1beta/models/gemini-test:streamGenerateContent?alt=sse", preStreamGeminiBody)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

		heartbeatAt := strings.Index(body, ": keep-alive\n\n")
		dataAt := strings.Index(body, "data:")
		require.GreaterOrEqual(t, heartbeatAt, 0, "expected a pre-stream SSE heartbeat, got %q", body)
		require.GreaterOrEqual(t, dataAt, 0, "expected Gemini SSE data, got %q", body)
		require.Less(t, heartbeatAt, dataAt, "heartbeat must precede the first upstream event")
		require.Contains(t, body, "candidates")
	})

	t.Run("streamGenerateContent default JSON array", func(t *testing.T) {
		fixture := newPreStreamFixture(t, llm.APIFormatGeminiContents, preStreamGeminiModel,
			preStreamGeminiSSEUpstream(200*time.Millisecond))

		// Both heartbeat families are enabled: the JSON array stream must still stay silent.
		resp, body := fixture.runGemini(t, preStreamHandlerOptions{
			inbound:   "gemini",
			sse:       SSEKeepAliveConfig{Enabled: true, Interval: 20 * time.Millisecond},
			nonStream: NonStreamKeepAliveConfig{Enabled: true, Interval: 20 * time.Millisecond},
		}, "/v1beta/models/gemini-test:streamGenerateContent", preStreamGeminiBody)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NotContains(t, body, ": keep-alive")
		require.True(t, strings.HasPrefix(body, "["), "expected a JSON array body, got %q", body)
		require.True(t, strings.HasSuffix(strings.TrimSpace(body), "]"), "expected a closed JSON array, got %q", body)

		var chunks []map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &chunks))
		require.NotEmpty(t, chunks)
	})

	t.Run("generateContent non-stream", func(t *testing.T) {
		fixture := newPreStreamFixture(t, llm.APIFormatGeminiContents, preStreamGeminiModel,
			preStreamDelayedUpstream(300*time.Millisecond, http.StatusOK, "application/json", preStreamGeminiResponse))

		resp, body := fixture.runGemini(t, preStreamHandlerOptions{
			inbound:   "gemini",
			nonStream: NonStreamKeepAliveConfig{Enabled: true, Interval: 20 * time.Millisecond},
		}, "/v1beta/models/gemini-test:generateContent", preStreamGeminiBody)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, resp.Header.Get("Content-Type"), "application/json")

		payload := strings.TrimLeft(body, "\n")
		require.Less(t, len(payload), len(body), "expected a non-stream heartbeat, got %q", body)

		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(payload), &decoded))
		require.Contains(t, decoded, "candidates")
	})
}
