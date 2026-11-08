package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

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
	"github.com/looplj/axonhub/llm/transformer/openai"
)

const (
	nonStreamChatRequestBody = `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`
	nonStreamChatResponse    = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}}`
	nonStreamErrorResponse   = `{"error":{"message":"upstream exploded","type":"server_error"}}`
)

type nonStreamKeepAliveSelector struct {
	candidates []*orchestrator.ChannelModelsCandidate
}

func (s *nonStreamKeepAliveSelector) Select(context.Context, *llm.Request) ([]*orchestrator.ChannelModelsCandidate, error) {
	return s.candidates, nil
}

// nonStreamKeepAliveFixture drives the real chat completion handler against a rewriting
// httptest upstream that answers non-streaming requests after a delay.
type nonStreamKeepAliveFixture struct {
	ctx      context.Context
	client   *ent.Client
	channel  *ent.Channel
	upstream *httptest.Server
}

func newNonStreamKeepAliveFixture(t *testing.T, status int, payload string, delay time.Duration) *nonStreamKeepAliveFixture {
	t.Helper()

	dbName := strings.ReplaceAll(t.Name(), "/", "_")
	client := enttest.NewEntClient(t, "sqlite3", "file:non-stream-keepalive-"+dbName+"?mode=memory&_fk=0")
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), client))
	project, err := client.Project.Create().SetName("non-stream-keepalive").Save(ctx)
	require.NoError(t, err)
	_, err = client.DataStorage.Create().
		SetName("non-stream-keepalive-storage").
		SetDescription("non-stream keep-alive test storage").
		SetPrimary(true).
		SetType(datastorage.TypeDatabase).
		SetSettings(&objects.DataStorageSettings{}).
		SetStatus(datastorage.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		//nolint:errcheck // the test client owns the read side of this body
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(upstream.Close)

	channelRow, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("non-stream-keepalive-channel").
		SetBaseURL(upstream.URL).
		SetCredentials(objects.ChannelCredentials{APIKey: "synthetic"}).
		SetSupportedModels([]string{"gpt-test"}).
		SetDefaultTestModel("gpt-test").
		SetStatus(channel.StatusEnabled).
		SetEndpoints([]objects.ChannelEndpoint{{
			APIFormat: llm.APIFormatOpenAIChatCompletion.String(),
			BaseURL:   upstream.URL,
			Transport: objects.ChannelEndpointTransportHTTP,
		}}).
		Save(ctx)
	require.NoError(t, err)

	// Created once per fixture: the price is unique per channel/model and every handler
	// built from this fixture must see the same row.
	price := decimal.NewFromFloat(0.01)
	_, err = client.ChannelModelPrice.Create().
		SetChannelID(channelRow.ID).
		SetModelID("gpt-test").
		SetReferenceID("non-stream-keepalive-price").
		SetPrice(objects.ModelPrice{Items: []objects.ModelPriceItem{
			{ItemCode: objects.PriceItemCodeUsage, Pricing: objects.Pricing{Mode: objects.PricingModeUsagePerUnit, UsagePerUnit: &price}},
			{ItemCode: objects.PriceItemCodeCompletion, Pricing: objects.Pricing{Mode: objects.PricingModeUsagePerUnit, UsagePerUnit: &price}},
		}}).
		Save(ctx)
	require.NoError(t, err)

	return &nonStreamKeepAliveFixture{
		ctx:      contexts.WithProjectID(ctx, project.ID),
		client:   client,
		channel:  channelRow,
		upstream: upstream,
	}
}

func (f *nonStreamKeepAliveFixture) handler(t *testing.T, cfg NonStreamKeepAliveConfig) *ChatCompletionHandlers {
	t.Helper()

	return f.handlerWithProvider(t, cfg, nil)
}

// handlerWithProvider builds the same handler stack, wired to a keep-alive provider the way
// the fx-injected lanes receive WebUI overrides.
func (f *nonStreamKeepAliveFixture) handlerWithProvider(
	t *testing.T,
	cfg NonStreamKeepAliveConfig,
	provider func(context.Context) (biz.KeepAliveSettings, error),
) *ChatCompletionHandlers {
	t.Helper()

	channelService, requestService, systemService, usageService := setupSpeechAPITestServices(t, f.client)
	promptService := biz.NewPromptService(biz.PromptServiceParams{Ent: f.client})
	storage, err := f.client.DataStorage.Query().Only(f.ctx)
	require.NoError(t, err)
	require.NoError(t, systemService.SetDefaultDataStorageID(f.ctx, storage.ID))
	// Keep the failure case single-shot so an upstream error surfaces without retry waits.
	require.NoError(t, systemService.SetRetryPolicy(f.ctx, &biz.RetryPolicy{
		Enabled:                 true,
		MaxChannelRetries:       0,
		MaxSingleChannelRetries: 0,
		RetryDelayMs:            0,
		LoadBalancerStrategy:    biz.LoadBalancerStrategyAdaptive,
	}))
	promptProtection := biz.NewPromptProtectionRuleService(biz.PromptProtectionRuleServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		Ent:         f.client,
	})
	t.Cleanup(promptProtection.Stop)

	built, err := channelService.GetChannel(f.ctx, f.channel.ID)
	require.NoError(t, err)
	channelService.PreloadModelPricesForTest(f.ctx, built)
	channelService.SetEnabledChannelsForTest([]*biz.Channel{built})

	selector := &nonStreamKeepAliveSelector{candidates: []*orchestrator.ChannelModelsCandidate{{
		Channel: built,
		Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-test", ActualModel: "gpt-test", Source: "direct"}},
	}}}
	orch := orchestrator.NewChatCompletionOrchestrator(
		channelService,
		nil,
		requestService,
		httpclient.NewHttpClient(),
		openai.NewInboundTransformer(),
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

	handlers := NewChatCompletionHandlers(orch)
	handlers.nonStreamKeepAlive = cfg
	handlers.keepAliveConfigProvider = provider

	return handlers
}

func (f *nonStreamKeepAliveFixture) testContext(c *gin.Context) {
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

// run posts a non-streaming chat completion and returns the response with its raw body.
func (f *nonStreamKeepAliveFixture) run(t *testing.T, cfg NonStreamKeepAliveConfig) (*http.Response, string) {
	t.Helper()

	return f.runWithProvider(t, cfg, nil)
}

// runWithProvider posts a non-streaming chat completion through a handler whose keep-alive
// settings come from the given provider.
func (f *nonStreamKeepAliveFixture) runWithProvider(
	t *testing.T,
	cfg NonStreamKeepAliveConfig,
	provider func(context.Context) (biz.KeepAliveSettings, error),
) (*http.Response, string) {
	t.Helper()

	handlers := f.handlerWithProvider(t, cfg, provider)
	engine := gin.New()
	engine.Use(f.testContext)
	engine.POST("/v1/chat/completions", handlers.ChatCompletion)
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(nonStreamChatRequestBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp, string(body)
}

func TestNonStreamKeepAliveSlowUpstreamPrefixesHeartbeats(t *testing.T) {
	fixture := newNonStreamKeepAliveFixture(t, http.StatusOK, nonStreamChatResponse, 300*time.Millisecond)

	resp, body := fixture.run(t, NonStreamKeepAliveConfig{Enabled: true, Interval: 20 * time.Millisecond})

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	payload := strings.TrimLeft(body, "\n")
	require.Less(t, len(payload), len(body), "expected heartbeat bytes before the JSON body, got %q", body)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &decoded))
	require.Equal(t, "chat.completion", decoded["object"])
}

// TestNonStreamKeepAliveInactiveKeepsResponseUnchanged asserts that a disabled feature and
// a misconfigured one (enabled without an interval) leave the response untouched: no
// leading bytes and the upstream status/content type are forwarded as before.
func TestNonStreamKeepAliveInactiveKeepsResponseUnchanged(t *testing.T) {
	fixture := newNonStreamKeepAliveFixture(t, http.StatusOK, nonStreamChatResponse, 100*time.Millisecond)

	tests := []struct {
		name string
		cfg  NonStreamKeepAliveConfig
	}{
		{name: "disabled", cfg: NonStreamKeepAliveConfig{}},
		{name: "enabled without interval", cfg: NonStreamKeepAliveConfig{Enabled: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := fixture.run(t, tt.cfg)

			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Contains(t, resp.Header.Get("Content-Type"), "application/json")
			require.False(t, strings.HasPrefix(body, "\n"), "expected no heartbeat bytes, got %q", body)

			var decoded map[string]any
			require.NoError(t, json.Unmarshal([]byte(body), &decoded))
			require.Equal(t, "chat.completion", decoded["object"])
		})
	}
}

// TestNonStreamKeepAliveFastUpstreamWritesNoBytes covers the zero-impact promise: when the
// upstream answers before the first tick, the response must be byte-identical to the plain
// path (no leading whitespace, upstream JSON untouched).
func TestNonStreamKeepAliveFastUpstreamWritesNoBytes(t *testing.T) {
	fixture := newNonStreamKeepAliveFixture(t, http.StatusOK, nonStreamChatResponse, 10*time.Millisecond)

	// An interval this large cannot tick within the test, so any heartbeat byte would be a
	// defect in the start/stop handshake rather than a timing race.
	resp, body := fixture.run(t, NonStreamKeepAliveConfig{Enabled: true, Interval: time.Hour})

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "application/json")
	require.NotContains(t, body, "\n", "expected no heartbeat bytes, got %q", body)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &decoded))
	require.Equal(t, "chat.completion", decoded["object"])
}

func TestNonStreamKeepAliveUpstreamErrorAfterHeartbeatKeepsStatus200(t *testing.T) {
	fixture := newNonStreamKeepAliveFixture(t, http.StatusInternalServerError, nonStreamErrorResponse, 200*time.Millisecond)

	resp, body := fixture.run(t, NonStreamKeepAliveConfig{Enabled: true, Interval: 20 * time.Millisecond})

	// The heartbeat already committed the status line, so the error can only travel in the body.
	require.Equal(t, http.StatusOK, resp.StatusCode)

	payload := strings.TrimLeft(body, "\n")
	require.Less(t, len(payload), len(body), "expected heartbeat bytes before the error body, got %q", body)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &decoded))
	require.Contains(t, decoded, "error")
	// The committed body must carry the real upstream error, not a generic gateway error.
	require.Contains(t, payload, "upstream exploded")
}

func TestNonStreamKeepAliveUpstreamErrorWithoutHeartbeatKeepsStatus(t *testing.T) {
	fixture := newNonStreamKeepAliveFixture(t, http.StatusInternalServerError, nonStreamErrorResponse, 100*time.Millisecond)

	resp, body := fixture.run(t, NonStreamKeepAliveConfig{})

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	require.False(t, strings.HasPrefix(body, "\n"), "expected no heartbeat bytes, got %q", body)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &decoded))
	require.Contains(t, decoded, "error")
}

// TestNonStreamKeepAliveProviderSettingsTakeEffect proves the runtime settings provider wins
// over the static handler fields: the handler is built with the feature disabled (the YAML
// default), yet the provider enables it and the slow upstream still yields heartbeat bytes
// downstream. A static-only implementation would emit no leading "\n" and fail here.
func TestNonStreamKeepAliveProviderSettingsTakeEffect(t *testing.T) {
	fixture := newNonStreamKeepAliveFixture(t, http.StatusOK, nonStreamChatResponse, 300*time.Millisecond)

	provider := func(context.Context) (biz.KeepAliveSettings, error) {
		return biz.KeepAliveSettings{
			NonStreaming: biz.KeepAliveConfig{Enabled: true, IntervalSeconds: 0.02},
			Streaming:    biz.KeepAliveConfig{Enabled: false, IntervalSeconds: 15},
		}, nil
	}

	resp, body := fixture.runWithProvider(t, NonStreamKeepAliveConfig{}, provider)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "application/json")
	require.True(t, strings.HasPrefix(body, "\n"),
		"expected heartbeat bytes from the provider settings, got %q", body)

	payload := strings.TrimLeft(body, "\n")
	require.Less(t, len(payload), len(body), "expected heartbeat bytes before the JSON body, got %q", body)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &decoded))
	require.Equal(t, "chat.completion", decoded["object"])
}

// TestNonStreamKeepAliveProviderErrorFallsBackToStatic asserts the failure path: a provider
// error must not silently disable heartbeats, the handler keeps using the static config it
// was built with (Enabled + 20ms here), so the response still carries the "\n" prefix.
func TestNonStreamKeepAliveProviderErrorFallsBackToStatic(t *testing.T) {
	fixture := newNonStreamKeepAliveFixture(t, http.StatusOK, nonStreamChatResponse, 300*time.Millisecond)

	provider := func(context.Context) (biz.KeepAliveSettings, error) {
		return biz.KeepAliveSettings{}, errors.New("settings store unavailable")
	}

	static := NonStreamKeepAliveConfig{Enabled: true, Interval: 20 * time.Millisecond}
	resp, body := fixture.runWithProvider(t, static, provider)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "application/json")
	require.True(t, strings.HasPrefix(body, "\n"),
		"expected heartbeat bytes from the static fallback, got %q", body)

	payload := strings.TrimLeft(body, "\n")
	require.Less(t, len(payload), len(body), "expected heartbeat bytes before the JSON body, got %q", body)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &decoded))
	require.Equal(t, "chat.completion", decoded["object"])
}

func TestIsNonStreamingRequest(t *testing.T) {
	tests := []struct {
		name string
		req  *httpclient.Request
		want bool
	}{
		{name: "nil request", req: nil},
		{
			name: "non-stream chat",
			req:  &httpclient.Request{Path: "/v1/chat/completions", Body: []byte(`{"model":"gpt-test"}`)},
			want: true,
		},
		{
			name: "explicit stream false",
			req:  &httpclient.Request{Path: "/v1/chat/completions", Body: []byte(`{"model":"gpt-test","stream":false}`)},
			want: true,
		},
		{
			name: "streaming chat",
			req:  &httpclient.Request{Path: "/v1/chat/completions", Body: []byte(`{"model":"gpt-test","stream":true}`)},
		},
		{
			name: "gemini streaming path",
			req:  &httpclient.Request{Path: "/v1beta/models/gpt-test:streamGenerateContent", Body: []byte(`{"contents":[]}`)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isNonStreamingRequest(tt.req))
		})
	}
}
