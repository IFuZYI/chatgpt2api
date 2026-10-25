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
	ListAbnormalTokens() []string
	ListRefreshTokenKeepaliveTokens() []string
	RefreshAccounts(ctx context.Context, tokens []string, progressID string, verify RefreshVerifier) map[string]any
	KeepaliveRefreshTokens(ctx context.Context, tokens []string, refresh TokenRefreshFunc) map[string]any
}

// TokenRefreshFunc 用 refresh_token 换一套新 token。
//
// 返回 map[string]string 而不是单个字符串，因为它必须带回**轮换后的新 refresh_token**。
// 上游每次换 AT 都会返回一个新 RT（实测 196 → 211 字符），只取 access_token
// 会把新 RT 丢掉，账号一直用旧 RT 续期。
type TokenRefreshFunc func(ctx context.Context, refreshToken string) (map[string]string, error)

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
func (w *AccountWatcher) Start(verify RefreshVerifier, refresh TokenRefreshFunc) {
	go w.run(verify, refresh)
}

// Stop 停止循环并等待退出。
func (w *AccountWatcher) Stop() {
	w.once.Do(func() { close(w.stop) })
	<-w.done
}

func (w *AccountWatcher) run(verify RefreshVerifier, refresh TokenRefreshFunc) {
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
func (w *AccountWatcher) tick(verify RefreshVerifier, refresh TokenRefreshFunc) {
	// 单轮 panic 不能杀死整个 watcher goroutine。
	//
	// 后果对比：没有 recover 时，一次 panic（上游返回畸形数据、空指针、
	// 并发 map 访问…）会让 run() 退出，进程还活着但限流账号**永不自动恢复**，
	// 且日志里只有一次 goroutine panic，之后完全静默。
	// Python 对应实现（api/support.py:87-109）是整轮 try/except。
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Printf("[account-watcher] tick panicked, recovered: %v\n", recovered)
		}
	}()

	// 单轮最多 30 分钟，避免卡死整个循环。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	limited := w.accounts.ListLimitedTokens()
	normal := w.accounts.ListNormalTokens()
	expiring := w.accounts.ListExpiringAccessTokens()
	abnormal := w.accounts.ListAbnormalTokens()
	keepalive := w.accounts.ListRefreshTokenKeepaliveTokens()

	// 去重合并（Python 用 dict.fromkeys 保序）。
	seen := map[string]bool{}
	tokens := make([]string, 0, len(limited)+len(normal)+len(expiring)+len(abnormal))
	for _, group := range [][]string{limited, normal, expiring, abnormal} {
		for _, token := range group {
			if token == "" || seen[token] {
				continue
			}
			seen[token] = true
			tokens = append(tokens, token)
		}
	}

	// keepalive **优先**：它与 refresh_accounts 不是等价路径。
	//
	//   refresh_accounts → FetchAccountInfo，只用 access_token 读元信息，**从不续期**
	//   keepalive        → RefreshAccessToken，用 refresh_token 换回新 AT + 新 RT
	//
	// 所以 keepalive 是超集。原来的实现反过来（把 expiring 从 keepalive 里剔除），
	// 后果是：一个 AT 进入 24h 窗口、同时 RT 锚点已超 3 天的账号，
	// 唯一能救它的路径被禁掉，交给了「AT 一过期就 401」的那条 ——
	// 账号被标异常（auto_remove 打开时直接删除），而它的 RT 明明还能用。
	//
	// 现在反过来：keepalive 名单里的 token 从 tokens 里剔除，交给 keepalive 处理。
	pendingSet := map[string]bool{}
	for _, token := range keepalive {
		pendingSet[token] = true
	}
	if len(pendingSet) > 0 {
		filtered := tokens[:0]
		for _, token := range tokens {
			if pendingSet[token] {
				continue
			}
			filtered = append(filtered, token)
		}
		tokens = filtered
	}
	pending := keepalive

	if len(tokens) > 0 {
		fmt.Printf("[account-watcher] checking %d limited, %d normal, %d expiring, %d abnormal\n",
			len(limited), len(normal), len(expiring), len(abnormal))
		// progressID 传空：watcher 不对外暴露进度。
		w.accounts.RefreshAccounts(ctx, tokens, "", verify)
	}
	if len(pending) > 0 {
		fmt.Printf("[account-watcher] keepalive %d refresh tokens\n", len(pending))
		result := w.accounts.KeepaliveRefreshTokens(ctx, pending, refresh)
		if relogined := intValue(result["relogined"]); relogined > 0 {
			fmt.Printf("[account-watcher] keepalive: refresh_token 被拒，已用密码重登恢复 %d 个账号\n", relogined)
		}
		if errorsOut, ok := result["errors"].([]any); ok && len(errorsOut) > 0 {
			fmt.Printf("[account-watcher] keepalive errors: %v\n", errorsOut)
		}
	}
}
