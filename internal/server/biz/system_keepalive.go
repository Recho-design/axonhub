package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/xerrors"
	"github.com/looplj/axonhub/internal/scopes"
)

const SystemKeyKeepAlive = "keep_alive_settings"

// DefaultKeepAliveIntervalSeconds is used when the injected server default does
// not carry a usable interval (for example when no YAML value was configured).
const DefaultKeepAliveIntervalSeconds = 15

// KeepAliveConfig uses seconds at the settings API boundary, like the other
// duration-carrying system settings.
type KeepAliveConfig struct {
	Enabled         bool    `json:"enabled"`
	IntervalSeconds float64 `json:"intervalSeconds"`
}

// KeepAliveDefaults carries the YAML server defaults (server.sse_keep_alive and
// server.non_stream_keep_alive) into the system service at construction time.
type KeepAliveDefaults struct {
	Streaming    KeepAliveConfig
	NonStreaming KeepAliveConfig
}

type KeepAliveSettings struct {
	Streaming           KeepAliveConfig `json:"streaming"`
	NonStreaming        KeepAliveConfig `json:"nonStreaming"`
	UsingServerDefaults bool            `json:"usingServerDefaults"`
}

// keepAliveStored is the persisted payload: the server-default flag is derived
// on read and never stored.
type keepAliveStored struct {
	Streaming    KeepAliveConfig `json:"streaming"`
	NonStreaming KeepAliveConfig `json:"nonStreaming"`
}

// validate rejects intervals that cannot be used to schedule a ticker. Disabled
// sections are validated too, so a later enable cannot inherit a broken value.
func (c KeepAliveConfig) validate(section string) error {
	if math.IsNaN(c.IntervalSeconds) || math.IsInf(c.IntervalSeconds, 0) || c.IntervalSeconds <= 0 {
		return xerrors.ValidationError(section + " keep-alive interval must be a finite number of seconds greater than zero")
	}

	return nil
}

// storable validates the input and drops the derived flag before persistence.
func (s KeepAliveSettings) storable() (keepAliveStored, error) {
	if err := s.Streaming.validate("streaming"); err != nil {
		return keepAliveStored{}, err
	}
	if err := s.NonStreaming.validate("non-streaming"); err != nil {
		return keepAliveStored{}, err
	}

	return keepAliveStored{Streaming: s.Streaming, NonStreaming: s.NonStreaming}, nil
}

// keepAliveDefaultConfig falls back to the built-in 15s interval for injected
// defaults that are unusable, keeping the configured enabled flag.
func keepAliveDefaultConfig(defaults KeepAliveConfig) KeepAliveConfig {
	if math.IsNaN(defaults.IntervalSeconds) || math.IsInf(defaults.IntervalSeconds, 0) || defaults.IntervalSeconds <= 0 {
		return KeepAliveConfig{Enabled: defaults.Enabled, IntervalSeconds: DefaultKeepAliveIntervalSeconds}
	}

	return defaults
}

// serverDefaultKeepAliveSettings reports the effective server defaults.
func (s *SystemService) serverDefaultKeepAliveSettings() KeepAliveSettings {
	return KeepAliveSettings{
		Streaming:           keepAliveDefaultConfig(s.keepAliveDefaults.Streaming),
		NonStreaming:        keepAliveDefaultConfig(s.keepAliveDefaults.NonStreaming),
		UsingServerDefaults: true,
	}
}

func (s *SystemService) keepAliveSettings(ctx context.Context) (KeepAliveSettings, error) {
	// Read directly so an older in-flight cache fill, or another process's local
	// cache, cannot restore stale keep-alive settings after an administrator
	// saves or resets them.
	record, err := s.entFromContext(ctx).System.Query().Where(system.KeyEQ(SystemKeyKeepAlive)).Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return KeepAliveSettings{}, fmt.Errorf("read keep-alive settings: %w", err)
	}

	result := s.serverDefaultKeepAliveSettings()
	if ent.IsNotFound(err) || record.Value == "null" {
		return result, nil
	}

	var stored keepAliveStored
	if err := json.Unmarshal([]byte(record.Value), &stored); err != nil {
		return KeepAliveSettings{}, fmt.Errorf("decode keep-alive settings: %w", err)
	}
	// A stored row was validated on write; revalidate so a hand-edited or
	// downgraded row cannot silently produce a zero interval downstream.
	if err := stored.Streaming.validate("streaming"); err != nil {
		return KeepAliveSettings{}, err
	}
	if err := stored.NonStreaming.validate("non-streaming"); err != nil {
		return KeepAliveSettings{}, err
	}

	result.Streaming = stored.Streaming
	result.NonStreaming = stored.NonStreaming
	result.UsingServerDefaults = false

	return result, nil
}

func (s *SystemService) KeepAliveSettings(ctx context.Context) (*KeepAliveSettings, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeReadSettings); err != nil {
		return nil, err
	}

	settings, err := s.keepAliveSettings(ctx)
	if err != nil {
		return nil, err
	}

	return &settings, nil
}

// KeepAliveRuntimeConfig returns the effective keep-alive settings for request
// handling. Callers run with API-key or background contexts that carry no user
// principal, which the Ent privacy layer rejects, so the read uses a scoped
// system bypass. Keep-alive only affects wire heartbeats: an unreadable or
// corrupt override falls back to the server defaults instead of failing the
// request, and the failure is logged for operators.
func (s *SystemService) KeepAliveRuntimeConfig(ctx context.Context) KeepAliveSettings {
	settings, err := authz.RunWithSystemBypass(ctx, "system-keep-alive-runtime-config", func(bypassCtx context.Context) (KeepAliveSettings, error) {
		return s.keepAliveSettings(bypassCtx)
	})
	if err != nil {
		log.Warn(ctx, "failed to read keep-alive settings, using server defaults", log.Cause(err))

		return s.serverDefaultKeepAliveSettings()
	}

	return settings
}

// SetKeepAliveSettings persists both sections as one aggregated JSON value.
// UsingServerDefaults in the input is ignored because it is derived on read.
func (s *SystemService) SetKeepAliveSettings(ctx context.Context, input KeepAliveSettings) error {
	if err := authz.RequireScope(ctx, scopes.ScopeWriteSettings); err != nil {
		return err
	}

	stored, err := input.storable()
	if err != nil {
		return err
	}

	value, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("encode keep-alive settings: %w", err)
	}

	return s.setSystemValue(ctx, SystemKeyKeepAlive, string(value))
}

func (s *SystemService) ResetKeepAliveSettings(ctx context.Context) error {
	if err := authz.RequireScope(ctx, scopes.ScopeWriteSettings); err != nil {
		return err
	}
	// A null override keeps reset on the standard atomic upsert/cache invalidation path.
	return s.setSystemValue(ctx, SystemKeyKeepAlive, "null")
}
