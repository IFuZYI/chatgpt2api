package service

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// AccountWatcher 复刻 Python api/support.py 的 start_limited_account_watcher。
//
// 职责：周期性刷新「限流账号 + 正常账号 + 即将过期的 access_token」，
// 并对临近闲置上限的 refresh_token 做 keepalive。
//
// 为什么必须有它：号池里的「限流」状态只有靠这一轮刷新才能解除。
// 缺了它，被限流的账号在 restore_at 到期后**永远不会自动恢复**，
// 只能人工点刷新 —— 号池会随时间推移逐步失效。
type AccountWatcher struct {
	accounts AccountRefresher
	cfg      WatcherConfig

	stop chan struct{}
	once sync.Once
	done chan struct{}
}

// AccountRefresher 是 watcher 对号池的最小依赖。
type AccountRefresher interface {
	ListLimitedTokens() []string
	ListNormalTokens() []string
	ListExpiringAccessTokens() []string
	ListRefreshTokenKeepaliveTokens() []string
	RefreshAccounts(ctx context.Context, tokens []string, progressID string, verify RefreshVerifier) map[string]any
	KeepaliveRefreshTokens(ctx context.Context, tokens []string, refresh func(ctx context.Context, token string) (string, error)) map[string]any
}

// RefreshTokenFunc 用 refresh_token 换新的 access_token，返回新 token 字符串。
type RefreshTokenFunc func(ctx context.Context, refreshToken string) (string, error)

// WatcherConfig 是 watcher 需要的配置访问器。
type WatcherConfig interface {
	RefreshAccountIntervalMinute() int
}

// NewAccountWatcher 构造。
func NewAccountWatcher(accounts AccountRefresher, cfg WatcherConfig) *AccountWatcher {
	return &AccountWatcher{
		accounts: accounts,
		cfg:      cfg,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start 启动后台循环。重复调用无副作用。
func (w *AccountWatcher) Start(verify RefreshVerifier, refresh func(ctx context.Context, token string) (string, error)) {
	go w.run(verify, refresh)
}

// Stop 停止循环并等待退出。
func (w *AccountWatcher) Stop() {
	w.once.Do(func() { close(w.stop) })
	<-w.done
}

func (w *AccountWatcher) run(verify RefreshVerifier, refresh func(ctx context.Context, token string) (string, error)) {
	defer close(w.done)

	intervalMinutes := w.cfg.RefreshAccountIntervalMinute()
	if intervalMinutes < 1 {
		intervalMinutes = 60
	}
	interval := time.Duration(intervalMinutes) * time.Minute

	// 启动后先跑一轮，再进入周期等待。
	w.tick(verify, refresh)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			w.tick(verify, refresh)
		}
	}
}

// tick 执行一轮刷新。
//
// 顺序与 Python 一致：
//  1. 合并 limited + normal + expiring（去重，保持出现顺序）
//  2. keepalive 列表要排除已过期的那批（它们走 refresh_accounts）
//  3. 先 refresh_accounts，再 keepalive
func (w *AccountWatcher) tick(verify RefreshVerifier, refresh func(ctx context.Context, token string) (string, error)) {
	// 单轮最多 30 分钟，避免卡死整个循环。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	limited := w.accounts.ListLimitedTokens()
	normal := w.accounts.ListNormalTokens()
	expiring := w.accounts.ListExpiringAccessTokens()
	keepalive := w.accounts.ListRefreshTokenKeepaliveTokens()

	// 去重合并（Python 用 dict.fromkeys 保序）。
	seen := map[string]bool{}
	tokens := make([]string, 0, len(limited)+len(normal)+len(expiring))
	for _, group := range [][]string{limited, normal, expiring} {
		for _, token := range group {
			if token == "" || seen[token] {
				continue
			}
			seen[token] = true
			tokens = append(tokens, token)
		}
	}

	// keepalive 排除已在 tokens 里的（避免同一 token 两种刷新路径）。
	expiringSet := map[string]bool{}
	for _, token := range expiring {
		expiringSet[token] = true
	}
	pending := make([]string, 0, len(keepalive))
	for _, token := range keepalive {
		if !expiringSet[token] {
			pending = append(pending, token)
		}
	}

	if len(tokens) > 0 {
		fmt.Printf("[account-watcher] checking %d limited, %d normal, %d expiring\n",
			len(limited), len(normal), len(expiring))
		// progressID 传空：watcher 不对外暴露进度。
		w.accounts.RefreshAccounts(ctx, tokens, "", verify)
	}
	if len(pending) > 0 {
		fmt.Printf("[account-watcher] keepalive %d refresh tokens\n", len(pending))
		result := w.accounts.KeepaliveRefreshTokens(ctx, pending, refresh)
		if errorsOut, ok := result["errors"].([]any); ok && len(errorsOut) > 0 {
			fmt.Printf("[account-watcher] keepalive errors: %v\n", errorsOut)
		}
	}
}
