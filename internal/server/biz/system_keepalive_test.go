package biz

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

func keepAliveTestDefaults() KeepAliveDefaults {
	return KeepAliveDefaults{
		Streaming:    KeepAliveConfig{Enabled: true, IntervalSeconds: 20},
		NonStreaming: KeepAliveConfig{Enabled: false, IntervalSeconds: 30},
	}
}

// 无记录或 null 记录时回退到注入的服务器默认值。
func TestKeepAliveDefaultsFallback(t *testing.T) {
	s, db := setupTestSystemService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer db.Close()
	s.keepAliveDefaults = keepAliveTestDefaults()
	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), db))

	got, err := s.KeepAliveSettings(ctx)
	require.NoError(t, err)
	require.True(t, got.UsingServerDefaults)
	require.Equal(t, KeepAliveConfig{Enabled: true, IntervalSeconds: 20}, got.Streaming)
	require.Equal(t, KeepAliveConfig{Enabled: false, IntervalSeconds: 30}, got.NonStreaming)

	// A service built without injected defaults falls back to disabled / 15s.
	bare := NewSystemService(SystemServiceParams{Ent: db, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}})
	got, err = bare.KeepAliveSettings(ctx)
	require.NoError(t, err)
	require.True(t, got.UsingServerDefaults)
	require.Equal(t, KeepAliveConfig{Enabled: false, IntervalSeconds: 15}, got.Streaming)
	require.Equal(t, KeepAliveConfig{Enabled: false, IntervalSeconds: 15}, got.NonStreaming)

	// An explicit null row reset by an earlier run is still server defaults.
	require.NoError(t, s.ResetKeepAliveSettings(ctx))
	got, err = s.KeepAliveSettings(ctx)
	require.NoError(t, err)
	require.True(t, got.UsingServerDefaults)
	require.Equal(t, KeepAliveConfig{Enabled: true, IntervalSeconds: 20}, got.Streaming)
}

// 保存后读到的是 DB 值，且新实例（无进程内状态）同样读到 DB 值。
func TestKeepAlivePersistence(t *testing.T) {
	s, db := setupTestSystemService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer db.Close()
	s.keepAliveDefaults = keepAliveTestDefaults()
	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), db))

	input := KeepAliveSettings{
		Streaming:    KeepAliveConfig{Enabled: false, IntervalSeconds: 7.5},
		NonStreaming: KeepAliveConfig{Enabled: true, IntervalSeconds: 2},
	}
	require.NoError(t, s.SetKeepAliveSettings(ctx, input))

	got, err := s.KeepAliveSettings(ctx)
	require.NoError(t, err)
	require.False(t, got.UsingServerDefaults)
	require.Equal(t, input.Streaming, got.Streaming)
	require.Equal(t, input.NonStreaming, got.NonStreaming)

	other := NewSystemService(SystemServiceParams{Ent: db, KeepAlive: s.keepAliveDefaults, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}})
	got, err = other.KeepAliveSettings(ctx)
	require.NoError(t, err)
	require.False(t, got.UsingServerDefaults)
	require.Equal(t, input.Streaming, got.Streaming)
	require.Equal(t, input.NonStreaming, got.NonStreaming)

	// The runtime entry reads the same persisted override without admin scopes.
	runtime := other.KeepAliveRuntimeConfig(ent.NewContext(t.Context(), db))
	require.False(t, runtime.UsingServerDefaults)
	require.Equal(t, input.Streaming, runtime.Streaming)
	require.Equal(t, input.NonStreaming, runtime.NonStreaming)
}

// Reset 写 null 后回退到服务器默认，且 runtime 入口同步回退。
func TestKeepAliveResetFallsBackToDefaults(t *testing.T) {
	s, db := setupTestSystemService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer db.Close()
	s.keepAliveDefaults = keepAliveTestDefaults()
	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), db))

	require.NoError(t, s.SetKeepAliveSettings(ctx, KeepAliveSettings{
		Streaming:    KeepAliveConfig{Enabled: false, IntervalSeconds: 5},
		NonStreaming: KeepAliveConfig{Enabled: true, IntervalSeconds: 5},
	}))
	require.NoError(t, s.ResetKeepAliveSettings(ctx))

	got, err := s.KeepAliveSettings(ctx)
	require.NoError(t, err)
	require.True(t, got.UsingServerDefaults)
	require.Equal(t, s.keepAliveDefaults.Streaming, got.Streaming)
	require.Equal(t, s.keepAliveDefaults.NonStreaming, got.NonStreaming)

	runtime := s.KeepAliveRuntimeConfig(ent.NewContext(t.Context(), db))
	require.True(t, runtime.UsingServerDefaults)
	require.Equal(t, s.keepAliveDefaults, KeepAliveDefaults{Streaming: runtime.Streaming, NonStreaming: runtime.NonStreaming})
}

// interval <= 0（或非有限数）必须拒绝；enabled=false 时 interval 仍要合法并照常保存。
func TestKeepAliveValidation(t *testing.T) {
	s, db := setupTestSystemService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer db.Close()
	s.keepAliveDefaults = keepAliveTestDefaults()
	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), db))

	valid := KeepAliveConfig{Enabled: true, IntervalSeconds: 15}
	badIntervals := []float64{0, -1, -0.001, math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, interval := range badIntervals {
		require.Error(t, s.SetKeepAliveSettings(ctx, KeepAliveSettings{
			Streaming:    KeepAliveConfig{Enabled: true, IntervalSeconds: interval},
			NonStreaming: valid,
		}))
		require.Error(t, s.SetKeepAliveSettings(ctx, KeepAliveSettings{
			Streaming:    valid,
			NonStreaming: KeepAliveConfig{Enabled: true, IntervalSeconds: interval},
		}))
	}

	// Nothing was persisted by the rejected writes.
	got, err := s.KeepAliveSettings(ctx)
	require.NoError(t, err)
	require.True(t, got.UsingServerDefaults)

	// A disabled section keeps its interval as given.
	input := KeepAliveSettings{
		Streaming:    KeepAliveConfig{Enabled: false, IntervalSeconds: 0.25},
		NonStreaming: valid,
	}
	require.NoError(t, s.SetKeepAliveSettings(ctx, input))
	got, err = s.KeepAliveSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, KeepAliveConfig{Enabled: false, IntervalSeconds: 0.25}, got.Streaming)
}
