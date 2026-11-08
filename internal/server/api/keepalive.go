package api

import (
	"context"
	"time"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
)

// keepAliveProviderFromSystemService adapts SystemService.KeepAliveRuntimeConfig to the
// handler provider contract. The biz reader already degrades to the server defaults, so it
// never reports an error today; the handler keeps the error path for providers that can.
func keepAliveProviderFromSystemService(systemService *biz.SystemService) func(context.Context) (biz.KeepAliveSettings, error) {
	if systemService == nil {
		return nil
	}

	return func(ctx context.Context) (biz.KeepAliveSettings, error) {
		return systemService.KeepAliveRuntimeConfig(ctx), nil
	}
}

// resolveKeepAlive returns the keep-alive configs for one request. The provider is called at
// most once per request; a missing provider or a provider error falls back to the static YAML
// config the handler was built with, so a settings failure never silently disables heartbeats.
func (handlers *ChatCompletionHandlers) resolveKeepAlive(ctx context.Context) (NonStreamKeepAliveConfig, SSEKeepAliveConfig) {
	if handlers.keepAliveConfigProvider == nil {
		return handlers.nonStreamKeepAlive, handlers.sseKeepAlive
	}

	settings, err := handlers.keepAliveConfigProvider(ctx)
	if err != nil {
		log.Warn(ctx, "Failed to load keep-alive settings, falling back to the configured defaults", log.Cause(err))

		return handlers.nonStreamKeepAlive, handlers.sseKeepAlive
	}

	return keepAliveNonStreamConfig(settings.NonStreaming), keepAliveSSEConfig(settings.Streaming)
}

// keepAliveNonStreamConfig converts a settings section into a non-streaming heartbeat config.
func keepAliveNonStreamConfig(cfg biz.KeepAliveConfig) NonStreamKeepAliveConfig {
	return NonStreamKeepAliveConfig{Enabled: cfg.Enabled, Interval: keepAliveInterval(cfg.IntervalSeconds)}
}

// keepAliveSSEConfig converts a settings section into a streaming heartbeat config.
func keepAliveSSEConfig(cfg biz.KeepAliveConfig) SSEKeepAliveConfig {
	return SSEKeepAliveConfig{Enabled: cfg.Enabled, Interval: keepAliveInterval(cfg.IntervalSeconds)}
}

// keepAliveInterval converts the seconds carried by the settings boundary into a duration.
// The settings reader rejects non-finite and non-positive intervals, so no clamping here.
func keepAliveInterval(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}
