package service

import (
	"context"
	"sync/atomic"
	"testing"
)

// 缓存命中时不应该遍历号池。
//
// 回归点：ensure() 每次都先调 activeGroups()（遍历全部账号构造 map），
// 即使签名没变也要付这个成本。269 账号时占 ListModels 总耗时的 64%。
// 用「ListAccounts 被调用次数」来断言，而不是耗时（耗时在 CI 上不稳）。
func TestListModelsCacheHitDoesNotScanAccounts(t *testing.T) {
	accounts := buildAccounts(50)
	counter := &countingLister{accounts: accounts}
	catalog := NewModelCatalog(counter, func(ctx context.Context, token string) ([]map[string]any, error) {
		return []map[string]any{{"id": "gpt-6"}}, nil
	})
	ctx := context.Background()

	// 首次：需要扫号池（要建签名）
	catalog.ListModels(ctx)
	firstScans := counter.calls.Load()
	if firstScans == 0 {
		t.Fatal("first call must scan accounts to build the signature")
	}

	// 后续 10 次：TTL 内、签名未变，不该再扫
	counter.calls.Store(0)
	for i := 0; i < 10; i++ {
		catalog.ListModels(ctx)
	}
	if got := counter.calls.Load(); got != 0 {
		t.Errorf("cache hit scanned accounts %d times, want 0 (signature check must be cheap)", got)
	}
}

// 号池变化时必须让缓存失效（不能为了省扫描而永远不检查）。
func TestListModelsInvalidatesWhenAccountsChange(t *testing.T) {
	accounts := buildAccounts(2)
	counter := &countingLister{accounts: accounts}
	fetchCount := 0
	catalog := NewModelCatalog(counter, func(ctx context.Context, token string) ([]map[string]any, error) {
		fetchCount++
		return []map[string]any{{"id": "gpt-6"}}, nil
	})
	ctx := context.Background()

	catalog.ListModels(ctx)
	before := fetchCount

	// 加一个账号 → 签名变化 → 必须重新拉
	counter.setAccounts(buildAccounts(3))
	catalog.ListModels(ctx)
	if fetchCount <= before {
		t.Error("adding an account must invalidate the cache and refetch")
	}
}

// countingLister 记录 ListAccounts 被调用的次数，并支持运行时替换账号集。
//
// 实现 VersionedLister：真实号池（AccountService）有这个能力，
// 替身必须同样实现，否则测的是慢路径而不是生产路径。
type countingLister struct {
	calls    atomic.Int64
	version  atomic.Uint64
	accounts []Account
}

func (c *countingLister) ListAccounts() []Account {
	c.calls.Add(1)
	return c.accounts
}

func (c *countingLister) Version() uint64 { return c.version.Load() }

func (c *countingLister) setAccounts(accounts []Account) {
	c.accounts = accounts
	c.version.Add(1)
}
