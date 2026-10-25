package service

import (
	"context"
	"sync/atomic"
	"testing"
)

// watcher 的 tick 必须从 panic 中恢复，否则一次 panic 就杀死整个 goroutine，
// 进程还活着但限流账号永不自动恢复（Python 是整轮 try/except）。
func TestWatcherTickRecoversFromPanic(t *testing.T) {
	counter := &panickingRefresher{}
	cfg := &stubWatcherConfig{intervalMinutes: 1}
	watcher := NewAccountWatcher(counter, cfg)

	// tick 内部 panic 不能冒泡到测试（否则测试进程崩）
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("tick panicked instead of recovering: %v", r)
			}
		}()
		watcher.tick(nil, nil)
	}()

	if counter.calls.Load() == 0 {
		t.Fatal("tick should still call the refresher before panicking")
	}
	if counter.calls.Load() != 1 {
		t.Errorf("refresher called %d times, want 1", counter.calls.Load())
	}
}

// panickingRefresher 第一次调用 panic，之后正常。
type panickingRefresher struct {
	calls  atomic.Int64
	onCall func(attempt int64)
}

func (p *panickingRefresher) ListLimitedTokens() []string               { return []string{"tok-1"} }
func (p *panickingRefresher) ListNormalTokens() []string                { return nil }
func (p *panickingRefresher) ListExpiringAccessTokens() []string        { return nil }
func (p *panickingRefresher) ListAbnormalTokens() []string              { return nil }
func (p *panickingRefresher) ListRefreshTokenKeepaliveTokens() []string { return nil }

func (p *panickingRefresher) RefreshAccounts(ctx context.Context, tokens []string, progressID string, verify RefreshVerifier) map[string]any {
	p.calls.Add(1)
	panic("boom from RefreshAccounts")
}

func (p *panickingRefresher) KeepaliveRefreshTokens(ctx context.Context, tokens []string, refresh TokenRefreshFunc) map[string]any {
	return map[string]any{}
}

type stubWatcherConfig struct{ intervalMinutes int }

func (s *stubWatcherConfig) RefreshAccountIntervalMinute() int { return s.intervalMinutes }
