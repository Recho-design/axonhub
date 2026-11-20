package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/gemini"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

const (
	preStreamOpenAIModel    = "gpt-test"
	preStreamAnthropicModel = "claude-test"
	preStreamOpenAIBody     = `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}],"stream":true}`
	preStreamAnthropicBody  = `{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`
	preStreamResponsesBody  = `{"model":"gpt-test","input":"hello","stream":true}`
)

// preStreamFixture drives the real chat handler against a controllable upstream.
type preStreamFixture struct {
	ctx     context.Context
	client  *ent.Client
	channel *ent.Channel
	model   string
}

func newPreStreamFixture(t *testing.T, format llm.APIFormat, model string, upstream http.HandlerFunc) *preStreamFixture {
	t.Helper()

	ctx, client := newPreStreamEntContext(t)
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	channelRow := newPreStreamChannel(t, ctx, client, format, model, server.URL)

	return &preStreamFixture{
		ctx:     ctx,
		client:  client,
		channel: channelRow,
		model:   model,
	}
}

// newPreStreamEntContext creates an isolated database and a project-scoped context.
func newPreStreamEntContext(t *testing.T) (context.Context, *ent.Client) {
	t.Helper()

	dbName := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	client := enttest.NewEntClient(t, "sqlite3", "file:pre-stream-"+dbName+"?mode=memory&_fk=0")
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), client))
	project, err := client.Project.Create().SetName("pre-stream-keepalive").Save(ctx)
	require.NoError(t, err)
	_, err = client.DataStorage.Create().
		SetName("pre-stream-keepalive-storage").
		SetDescription("pre-stream keep-alive test storage").
		SetPrimary(true).
		SetType(datastorage.TypeDatabase).
		SetSettings(&objects.DataStorageSettings{}).
		SetStatus(datastorage.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	return contexts.WithProjectID(ctx, project.ID), client
}

// newPreStreamChannel creates the upstream channel and its model price.
func newPreStreamChannel(
	t *testing.T,
	ctx context.Context,
	client *ent.Client,
	format llm.APIFormat,
	model string,
	baseURL string,
) *ent.Channel {
	t.Helper()

	channelType := channel.TypeOpenai
	switch format {
	case llm.APIFormatAnthropicMessage:
		channelType = channel.TypeAnthropic
	case llm.APIFormatOpenAIResponse:
		channelType = channel.TypeOpenaiResponses
	case llm.APIFormatGeminiContents:
		channelType = channel.TypeGemini
	}

	channelRow, err := client.Channel.Create().
		SetType(channelType).
		SetName("pre-stream-keepalive-channel").
		SetBaseURL(baseURL).
		SetCredentials(objects.ChannelCredentials{APIKey: "synthetic"}).
		SetSupportedModels([]string{model}).
		SetDefaultTestModel(model).
		SetStatus(channel.StatusEnabled).
		SetEndpoints([]objects.ChannelEndpoint{{
			APIFormat: format.String(),
			BaseURL:   baseURL,
			Transport: objects.ChannelEndpointTransportHTTP,
		}}).
		Save(ctx)
	require.NoError(t, err)

	createPreStreamPrice(t, ctx, client, channelRow.ID, model)

	return channelRow
}

func createPreStreamPrice(t *testing.T, ctx context.Context, client *ent.Client, channelID int, model string) {
	t.Helper()

	price := decimal.NewFromFloat(0.01)
	_, err := client.ChannelModelPrice.Create().
		SetChannelID(channelID).
		SetModelID(model).
		SetReferenceID("pre-stream-keepalive-price").
		SetPrice(objects.ModelPrice{Items: []objects.ModelPriceItem{
			{ItemCode: objects.PriceItemCodeUsage, Pricing: objects.Pricing{Mode: objects.PricingModeUsagePerUnit, UsagePerUnit: &price}},
			{ItemCode: objects.PriceItemCodeCompletion, Pricing: objects.Pricing{Mode: objects.PricingModeUsagePerUnit, UsagePerUnit: &price}},
		}}).
		Save(ctx)
	require.NoError(t, err)
}

type preStreamHandlerOptions struct {
	inbound     string // "openai" (default), "anthropic", "responses", "gemini"
	sse         SSEKeepAliveConfig
	nonStream   NonStreamKeepAliveConfig
	provider    func(context.Context) (biz.KeepAliveSettings, error)
	errorPolicy biz.UpstreamErrorPolicy
}

func (f *preStreamFixture) handler(t *testing.T, opts preStreamHandlerOptions) *ChatCompletionHandlers {
	t.Helper()

	orch := f.orchestrator(t, opts)
	h := NewChatCompletionHandlers(orch)
	// Production OpenAI/Anthropic handlers leave StreamWriter nil and use the SSE fallback.
	h.StreamWriter = nil
	h.sseKeepAlive = opts.sse
	h.nonStreamKeepAlive = opts.nonStream
	h.keepAliveConfigProvider = opts.provider
	h.sseHeartbeatFormat = sseHeartbeatOpenAI
	if opts.inbound == "anthropic" {
		h.sseHeartbeatFormat = sseHeartbeatAnthropic
	}
	if opts.inbound == "responses" {
		h.streamAdapterFactory = newResponsesStreamAdapter
	}

	return h
}

func preStreamInbound(name string) transformer.Inbound {
	switch name {
	case "anthropic":
		return anthropic.NewInboundTransformer()
	case "gemini":
		return gemini.NewInboundTransformer()
	case "responses":
		return responses.NewInboundTransformer()
	default:
		return openai.NewInboundTransformer()
	}
}

func (f *preStreamFixture) orchestrator(t *testing.T, opts preStreamHandlerOptions) *orchestrator.ChatCompletionOrchestrator {
	t.Helper()

	channelService, requestService, systemService, usageService := setupSpeechAPITestServices(t, f.client)
	promptService := biz.NewPromptService(biz.PromptServiceParams{Ent: f.client})
	storage, err := f.client.DataStorage.Query().Only(f.ctx)
	require.NoError(t, err)
	require.NoError(t, systemService.SetDefaultDataStorageID(f.ctx, storage.ID))
	// Single attempt: failures must surface without retry delays.
	require.NoError(t, systemService.SetRetryPolicy(f.ctx, &biz.RetryPolicy{
		Enabled:                 true,
		MaxChannelRetries:       0,
		MaxSingleChannelRetries: 0,
		RetryDelayMs:            0,
		LoadBalancerStrategy:    biz.LoadBalancerStrategyAdaptive,
		UpstreamErrorPolicy:     opts.errorPolicy,
	}))
	promptProtection := biz.NewPromptProtectionRuleService(biz.PromptProtectionRuleServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		Ent:         f.client,
	})
	t.Cleanup(promptProtection.Stop)

	selector := f.enableChannel(t, channelService)

	return orchestrator.NewChatCompletionOrchestrator(
		channelService,
		nil,
		requestService,
		httpclient.NewHttpClient(),
		preStreamInbound(opts.inbound),
		systemService,
		usageService,
		promptService,
		nil,
		promptProtection,
		biz.NewLiveStreamRegistry(),
		orchestrator.NewChannelLimiterManager(),
		nil,
		orchestrator.PluginIntegration{},
	).WithChannelSelector(selector)
}

func (f *preStreamFixture) enableChannel(t *testing.T, channelService *biz.ChannelService) *nonStreamKeepAliveSelector {
	t.Helper()

	built, err := channelService.GetChannel(f.ctx, f.channel.ID)
	require.NoError(t, err)
	channelService.PreloadModelPricesForTest(f.ctx, built)
	channelService.SetEnabledChannelsForTest([]*biz.Channel{built})

	return &nonStreamKeepAliveSelector{candidates: []*orchestrator.ChannelModelsCandidate{{
		Channel: built,
		Models:  []biz.ChannelModelEntry{{RequestModel: f.model, ActualModel: f.model, Source: "direct"}},
	}}}
}

func (f *preStreamFixture) testContext(c *gin.Context) {
	ctx := authz.WithTestBypass(c.Request.Context())
	ctx = ent.NewContext(ctx, f.client)
	projectID, ok := contexts.GetProjectID(f.ctx)
	if !ok {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Request = c.Request.WithContext(contexts.WithProjectID(ctx, projectID))
	c.Next()
}

func preStreamResponsesMiddleware(c *gin.Context) {
	c.Request = c.Request.WithContext(shared.WithResponsesAPI(c.Request.Context()))
	c.Next()
}

// run posts one request through the real handler and returns the full response.
func (f *preStreamFixture) run(
	t *testing.T,
	opts preStreamHandlerOptions,
	path string,
	body string,
) (*http.Response, string) {
	t.Helper()

	h := f.handler(t, opts)
	engine := gin.New()
	engine.Use(f.testContext)
	if opts.inbound == "responses" {
		engine.Use(preStreamResponsesMiddleware)
	}
	engine.POST(path, h.ChatCompletion)
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

func preStreamDelayedUpstream(delay time.Duration, status int, contentType, payload string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}

		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		//nolint:errcheck // the gateway owns the read side of this body
		_, _ = io.WriteString(w, payload)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func TestPreStreamKeepAliveOpenAISlowUpstream(t *testing.T) {
	fixture := newPreStreamFixture(t, llm.APIFormatOpenAIChatCompletion, preStreamOpenAIModel,
		preStreamDelayedUpstream(200*time.Millisecond, http.StatusOK, "text/event-stream", streamDeliveryChat(12, 34, true)))

	resp, body := fixture.run(t, preStreamHandlerOptions{
		sse: SSEKeepAliveConfig{Enabled: true, Interval: 20 * time.Millisecond},
	}, "/v1/chat/completions", preStreamOpenAIBody)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
	require.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))

	heartbeatAt := strings.Index(body, ": keep-alive\n\n")
	require.GreaterOrEqual(t, heartbeatAt, 0, "expected a pre-stream heartbeat, got %q", body)
	require.Less(t, heartbeatAt, strings.Index(body, "data:"), "heartbeat must precede the first upstream event")
	require.Contains(t, body, "[DONE]")
}

func TestPreStreamKeepAliveAnthropicSlowUpstream(t *testing.T) {
	fixture := newPreStreamFixture(t, llm.APIFormatAnthropicMessage, preStreamAnthropicModel,
		preStreamDelayedUpstream(200*time.Millisecond, http.StatusOK, "text/event-stream", streamDeliveryAnthropic(12, 34)))

	resp, body := fixture.run(t, preStreamHandlerOptions{
		inbound: "anthropic",
		sse:     SSEKeepAliveConfig{Enabled: true, Interval: 20 * time.Millisecond},
	}, "/v1/messages", preStreamAnthropicBody)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	ping := "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	pingAt := strings.Index(body, ping)
	require.GreaterOrEqual(t, pingAt, 0, "expected an Anthropic ping, got %q", body)
	require.Less(t, pingAt, strings.Index(body, "event:message_start"))
	require.Contains(t, body, "event:message_stop")
}

func TestPreStreamKeepAliveFastUpstreamWritesNoBytes(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		fixture := newPreStreamFixture(t, llm.APIFormatOpenAIChatCompletion, preStreamOpenAIModel,
			preStreamDelayedUpstream(0, http.StatusOK, "text/event-stream", streamDeliveryChat(12, 34, true)))

		// An interval this large cannot tick during the test.
		resp, body := fixture.run(t, preStreamHandlerOptions{
			sse: SSEKeepAliveConfig{Enabled: true, Interval: time.Hour},
		}, "/v1/chat/completions", preStreamOpenAIBody)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NotContains(t, body, ": keep-alive")
		require.Contains(t, body, "[DONE]")
	})

	t.Run("error", func(t *testing.T) {
		fixture := newPreStreamFixture(t, llm.APIFormatOpenAIChatCompletion, preStreamOpenAIModel,
			preStreamDelayedUpstream(0, http.StatusTooManyRequests, "application/json", `{"error":{"message":"rate limited","type":"rate_limit_error"}}`))

		resp, body := fixture.run(t, preStreamHandlerOptions{
			sse: SSEKeepAliveConfig{Enabled: true, Interval: time.Hour},
		}, "/v1/chat/completions", preStreamOpenAIBody)

		require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
		require.NotContains(t, resp.Header.Get("Content-Type"), "text/event-stream")
		require.NotContains(t, body, ": keep-alive")

		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &decoded))
		require.Contains(t, decoded, "error")
	})
}

func TestPreStreamKeepAliveOpenAISlowErrorAfterHeartbeat(t *testing.T) {
	fixture := newPreStreamFixture(t, llm.APIFormatOpenAIChatCompletion, preStreamOpenAIModel,
		preStreamDelayedUpstream(200*time.Millisecond, http.StatusInternalServerError, "application/json",
			`{"error":{"message":"upstream exploded","type":"server_error"}}`))

	resp, body := fixture.run(t, preStreamHandlerOptions{
		sse:         SSEKeepAliveConfig{Enabled: true, Interval: 20 * time.Millisecond},
		errorPolicy: biz.UpstreamErrorPolicy{Mode: biz.UpstreamErrorModeHidden},
	}, "/v1/chat/completions", preStreamOpenAIBody)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	heartbeatAt := strings.Index(body, ": keep-alive\n\n")
	errorAt := strings.Index(body, "event:error\n")
	require.GreaterOrEqual(t, heartbeatAt, 0, "expected a heartbeat, got %q", body)
	require.GreaterOrEqual(t, errorAt, 0, "expected an SSE error event, got %q", body)
	require.Less(t, heartbeatAt, errorAt)

	require.NotContains(t, body, "upstream exploded")
	require.Contains(t, body, biz.DefaultUpstreamErrorMessage)
}

func TestPreStreamKeepAliveResponsesSlowErrorAfterHeartbeat(t *testing.T) {
	fixture := newPreStreamFixture(t, llm.APIFormatOpenAIResponse, preStreamOpenAIModel,
		preStreamDelayedUpstream(200*time.Millisecond, http.StatusInternalServerError, "application/json",
			`{"error":{"message":"upstream exploded","type":"server_error"}}`))

	resp, body := fixture.run(t, preStreamHandlerOptions{
		inbound:     "responses",
		sse:         SSEKeepAliveConfig{Enabled: true, Interval: 20 * time.Millisecond},
		errorPolicy: biz.UpstreamErrorPolicy{Mode: biz.UpstreamErrorModeHidden},
	}, "/v1/responses", preStreamResponsesBody)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
	require.Contains(t, body, ": keep-alive\n\n")

	const marker = "event:error\ndata: "
	at := strings.Index(body, marker)
	require.GreaterOrEqual(t, at, 0, "expected a Responses error event, got %q", body)

	line := body[at+len(marker):]
	if end := strings.Index(line, "\n\n"); end >= 0 {
		line = line[:end]
	}

	require.Equal(t, "error", gjson.Get(line, "type").String())
	require.Equal(t, int64(0), gjson.Get(line, "sequence_number").Int())
	require.Equal(t, biz.DefaultUpstreamErrorMessage, gjson.Get(line, "message").String())
	require.NotContains(t, line, "upstream exploded")
}

// TestPreStreamErrorEncoderRedactsOnlyUpstreamErrors covers the boundary between a
// gateway-local failure and a real upstream failure once the heartbeat has committed
// the response: only the upstream error may be replaced by the hidden message.
func TestPreStreamErrorEncoderRedactsOnlyUpstreamErrors(t *testing.T) {
	ctx, systemService := setupUpstreamErrorPolicyTest(t, biz.UpstreamErrorPolicy{Mode: biz.UpstreamErrorModeHidden})

	upstreamErr := pipeline.WrapUpstreamError(&httpclient.Error{
		StatusCode: http.StatusInternalServerError,
		Status:     http.StatusText(http.StatusInternalServerError),
		Body:       []byte(`{"error":{"message":"upstream exploded","type":"server_error"}}`),
	})

	cases := []struct {
		name        string
		handlers    *ChatCompletionHandlers
		err         error
		wantMessage string
	}{
		{
			name:        "openai local error keeps its message",
			handlers:    &ChatCompletionHandlers{sseHeartbeatFormat: sseHeartbeatOpenAI},
			err:         errors.New("model not allowed"),
			wantMessage: "model not allowed",
		},
		{
			name:        "openai upstream error is redacted",
			handlers:    &ChatCompletionHandlers{sseHeartbeatFormat: sseHeartbeatOpenAI},
			err:         upstreamErr,
			wantMessage: biz.DefaultUpstreamErrorMessage,
		},
		{
			name:        "responses local error keeps its message",
			handlers:    &ChatCompletionHandlers{streamAdapterFactory: newResponsesStreamAdapter},
			err:         errors.New("model not allowed"),
			wantMessage: "model not allowed",
		},
		{
			name:        "responses upstream error is redacted",
			handlers:    &ChatCompletionHandlers{streamAdapterFactory: newResponsesStreamAdapter},
			err:         upstreamErr,
			wantMessage: biz.DefaultUpstreamErrorMessage,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			encodeErr, preparedErr := tt.handlers.preStreamErrorEncoder(ctx, systemService, tt.err)
			event, err := encodeErr(ctx, orchestrator.ClassifyUpstreamTransportError(preparedErr))
			require.NoError(t, err)

			messagePath := "error.message"
			if tt.handlers.streamAdapterFactory != nil {
				messagePath = "message"
			}

			require.Equal(t, tt.wantMessage, gjson.GetBytes(event.Data, messagePath).String())
			if tt.wantMessage == biz.DefaultUpstreamErrorMessage {
				require.NotContains(t, string(event.Data), "upstream exploded")
			}
		})
	}
}

func TestPreStreamKeepAliveProviderSettingsTakeEffect(t *testing.T) {
	fixture := newPreStreamFixture(t, llm.APIFormatOpenAIChatCompletion, preStreamOpenAIModel,
		preStreamDelayedUpstream(200*time.Millisecond, http.StatusOK, "text/event-stream", streamDeliveryChat(12, 34, true)))

	t.Run("provider enables", func(t *testing.T) {
		provider := func(context.Context) (biz.KeepAliveSettings, error) {
			return biz.KeepAliveSettings{
				Streaming:    biz.KeepAliveConfig{Enabled: true, IntervalSeconds: 0.02},
				NonStreaming: biz.KeepAliveConfig{Enabled: false, IntervalSeconds: 15},
			}, nil
		}

		// Static config is disabled: only the provider can enable the heartbeat.
		resp, body := fixture.run(t, preStreamHandlerOptions{provider: provider}, "/v1/chat/completions", preStreamOpenAIBody)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, body, ": keep-alive\n\n")
	})

	t.Run("provider disables", func(t *testing.T) {
		provider := func(context.Context) (biz.KeepAliveSettings, error) {
			return biz.KeepAliveSettings{
				Streaming:    biz.KeepAliveConfig{Enabled: false, IntervalSeconds: 15},
				NonStreaming: biz.KeepAliveConfig{Enabled: false, IntervalSeconds: 15},
			}, nil
		}

		// Static config would tick every 20ms; the provider must win and stay silent.
		resp, body := fixture.run(t, preStreamHandlerOptions{
			sse:      SSEKeepAliveConfig{Enabled: true, Interval: 20 * time.Millisecond},
			provider: provider,
		}, "/v1/chat/completions", preStreamOpenAIBody)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NotContains(t, body, ": keep-alive")
		require.Contains(t, body, "[DONE]")
	})
}

// preStreamFailingWriter fails every write, simulating a client that vanished while
// the pre-stream heartbeat is running.
type preStreamFailingWriter struct {
	gin.ResponseWriter

	failed chan struct{}
}

func (w *preStreamFailingWriter) Write(_ []byte) (int, error) {
	select {
	case w.failed <- struct{}{}:
	default:
	}

	return 0, io.ErrClosedPipe
}

func (w *preStreamFailingWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

// newPreStreamFailingContext builds a gin context whose writer always fails.
func newPreStreamFailingContext(
	t *testing.T,
	ctx context.Context,
	body string,
) (*gin.Context, *httpclient.Request, *preStreamFailingWriter) {
	t.Helper()

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	httpReq = httpReq.WithContext(ctx)
	httpReq.Header.Set("Content-Type", "application/json")
	genericReq, err := httpclient.ReadHTTPRequest(httpReq)
	require.NoError(t, err)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httpReq
	writer := &preStreamFailingWriter{ResponseWriter: c.Writer, failed: make(chan struct{}, 1)}
	c.Writer = writer

	return c, genericReq, writer
}

func TestPreStreamKeepAliveWriteFailureCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	upstreamDone := make(chan struct{})
	var once sync.Once

	fixture := newPreStreamFixture(t, llm.APIFormatOpenAIChatCompletion, preStreamOpenAIModel,
		func(w http.ResponseWriter, r *http.Request) {
			// Drain the request body so net/http can watch the connection for an
			// abort and cancel r.Context() when the gateway gives up.
			_, _ = io.Copy(io.Discard, r.Body)
			once.Do(func() { close(started) })
			<-r.Context().Done()
			close(upstreamDone)
		})

	h := fixture.handler(t, preStreamHandlerOptions{
		sse: SSEKeepAliveConfig{Enabled: true, Interval: 10 * time.Millisecond},
	})
	c, genericReq, writer := newPreStreamFailingContext(t, fixture.ctx, preStreamOpenAIBody)

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		h.ChatCompletionWithRequest(c, genericReq)
	}()

	waitPreStreamSignal(t, started, "upstream call did not start")
	waitPreStreamSignal(t, writer.failed, "pre-stream heartbeat did not attempt a write")

	// The failed heartbeat must cancel Process, which aborts the upstream request
	// instead of leaving it parked forever.
	waitPreStreamSignal(t, upstreamDone, "write failure did not cancel the upstream request")
	waitPreStreamSignal(t, handlerDone, "handler did not return after cancellation")
}

func waitPreStreamSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(message)
	}
}

type preProcessKeepAliveCase struct {
	name     string
	handlers *ChatCompletionHandlers
	req      *httpclient.Request
	want     preProcessKeepAliveMode
}

func TestClassifyPreProcessKeepAlive(t *testing.T) {
	for _, tt := range preProcessKeepAliveCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.handlers.classifyPreProcessKeepAlive(tt.req))
		})
	}
}

func preStreamOpenAIHandlers() *ChatCompletionHandlers {
	return &ChatCompletionHandlers{sseHeartbeatFormat: sseHeartbeatOpenAI}
}

func preStreamHandlersWithWriter(writer StreamWriter) *ChatCompletionHandlers {
	return &ChatCompletionHandlers{StreamWriter: writer, sseHeartbeatFormat: sseHeartbeatOpenAI}
}

func preStreamNoHeartbeatHandlers() *ChatCompletionHandlers {
	return &ChatCompletionHandlers{sseHeartbeatFormat: sseHeartbeatNone}
}

// keepAliveJSONCase builds one table row for a JSON request body.
func keepAliveJSONCase(
	name string,
	handlers *ChatCompletionHandlers,
	path string,
	body string,
	want preProcessKeepAliveMode,
) preProcessKeepAliveCase {
	return preProcessKeepAliveCase{
		name:     name,
		handlers: handlers,
		req:      &httpclient.Request{Path: path, Body: []byte(body)},
		want:     want,
	}
}

func preProcessKeepAliveCases(t *testing.T) []preProcessKeepAliveCase {
	t.Helper()

	cases := []preProcessKeepAliveCase{
		{name: "nil request", handlers: preStreamOpenAIHandlers(), req: nil, want: preProcessKeepAliveNone},
		keepAliveJSONCase("chat stream", preStreamOpenAIHandlers(),
			"/v1/chat/completions", `{"stream":true}`, preProcessKeepAliveSSE),
		keepAliveJSONCase("chat stream with binary writer", preStreamHandlersWithWriter(WriteBinaryStream),
			"/v1/chat/completions", `{"stream":true}`, preProcessKeepAliveNone),
		keepAliveJSONCase("chat stream without heartbeat protocol", preStreamNoHeartbeatHandlers(),
			"/v1/chat/completions", `{"stream":true}`, preProcessKeepAliveNone),
		keepAliveJSONCase("chat non-stream", preStreamOpenAIHandlers(),
			"/v1/chat/completions", `{"stream":false}`, preProcessKeepAliveNonStreamJSON),
		keepAliveJSONCase("gemini stream default JSON array", preStreamHandlersWithWriter(WriteGeminiStream),
			"/v1beta/models/gpt:streamGenerateContent", `{"contents":[]}`, preProcessKeepAliveNone),
		keepAliveGeminiCase("gemini stream alt=sse", preStreamOpenAIHandlers(), true, preProcessKeepAliveSSE),
		keepAliveGeminiCase("gemini stream alt=sse with JSON array writer",
			preStreamHandlersWithWriter(WriteGeminiStream), true, preProcessKeepAliveNone),
		keepAliveJSONCase("gemini generateContent non-stream", preStreamHandlersWithWriter(WriteGeminiStream),
			"/v1beta/models/gpt:generateContent", `{"contents":[]}`, preProcessKeepAliveNonStreamJSON),
		keepAliveJSONCase("responses compact with stream flag", preStreamOpenAIHandlers(),
			"/v1/responses/compact", `{"stream":true}`, preProcessKeepAliveNonStreamJSON),
		keepAliveJSONCase("responses compact with custom writer", preStreamHandlersWithWriter(WriteBinaryStream),
			"/v1/responses/compact", `{"stream":true}`, preProcessKeepAliveNone),
		keepAliveJSONCase("speech default binary", preStreamOpenAIHandlers(),
			"/v1/audio/speech", `{"input":"hi"}`, preProcessKeepAliveNone),
		keepAliveJSONCase("speech audio binary", preStreamOpenAIHandlers(),
			"/v1/audio/speech", `{"stream_format":"audio"}`, preProcessKeepAliveNone),
		keepAliveJSONCase("speech sse", preStreamOpenAIHandlers(),
			"/v1/audio/speech", `{"stream_format":" SSE "}`, preProcessKeepAliveSSE),
		keepAliveJSONCase("speech sse with binary writer", preStreamHandlersWithWriter(WriteBinaryStream),
			"/v1/audio/speech", `{"stream_format":"sse"}`, preProcessKeepAliveNone),
	}

	return append(cases, preProcessTranscriptionKeepAliveCases(t)...)
}

func preProcessTranscriptionKeepAliveCases(t *testing.T) []preProcessKeepAliveCase {
	t.Helper()

	transcriptionStream, transcriptionStreamType := preStreamMultipartBody(t, true, map[string]string{
		"stream": "true",
	})
	transcriptionJSON, transcriptionJSONType := preStreamMultipartBody(t, false, map[string]string{
		"stream":          "false",
		"response_format": "json",
	})
	transcriptionSRT, transcriptionSRTType := preStreamMultipartBody(t, false, map[string]string{
		"stream":          "false",
		"response_format": "srt",
	})

	return []preProcessKeepAliveCase{
		{
			name:     "transcription stream true",
			handlers: preStreamOpenAIHandlers(),
			req: &httpclient.Request{Path: "/v1/audio/transcriptions",
				Headers: http.Header{"Content-Type": {transcriptionStreamType}}, Body: transcriptionStream},
			want: preProcessKeepAliveSSE,
		},
		{
			name:     "transcription json",
			handlers: preStreamOpenAIHandlers(),
			req: &httpclient.Request{Path: "/v1/audio/transcriptions",
				Headers: http.Header{"Content-Type": {transcriptionJSONType}}, Body: transcriptionJSON},
			want: preProcessKeepAliveNonStreamJSON,
		},
		{
			name:     "transcription srt",
			handlers: preStreamOpenAIHandlers(),
			req: &httpclient.Request{Path: "/v1/audio/transcriptions",
				Headers: http.Header{"Content-Type": {transcriptionSRTType}}, Body: transcriptionSRT},
			want: preProcessKeepAliveNone,
		},
		{
			name:     "transcription malformed multipart",
			handlers: preStreamOpenAIHandlers(),
			req: &httpclient.Request{Path: "/v1/audio/transcriptions",
				Headers: http.Header{"Content-Type": {"multipart/form-data; boundary=missing"}}, Body: []byte("not a form")},
			want: preProcessKeepAliveNone,
		},
	}
}

// preStreamMultipartBody builds a transcription form. The file part is written first
// with a large payload so classification must skip it without buffering.
func preStreamMultipartBody(t *testing.T, fileFirst bool, fields map[string]string) ([]byte, string) {
	t.Helper()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	writeFile := func() {
		part, err := writer.CreateFormFile("file", "audio.mp3")
		require.NoError(t, err)
		_, err = part.Write(bytes.Repeat([]byte{0xff}, 64*1024))
		require.NoError(t, err)
	}

	if fileFirst {
		writeFile()
	}
	for key, value := range fields {
		require.NoError(t, writer.WriteField(key, value))
	}
	if !fileFirst {
		writeFile()
	}
	require.NoError(t, writer.Close())

	return buf.Bytes(), writer.FormDataContentType()
}
