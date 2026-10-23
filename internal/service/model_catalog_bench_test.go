package service

import (
	"context"
	"fmt"
	"testing"
)

// stubLister 构造 N 个账号的号池替身（实现 VersionedLister，模拟生产路径）。
type stubLister struct {
	accounts []Account
	version  uint64
}

func (s *stubLister) ListAccounts() []Account { return s.accounts }
func (s *stubLister) Version() uint64         { return s.version }

func buildAccounts(n int) []Account {
	out := make([]Account, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Account{
			"access_token": fmt.Sprintf("tok-%d", i),
			"type":         "free",
			"status":       StatusNormal,
			"quota":        25,
			"email":        fmt.Sprintf("u%d@example.com", i),
		})
	}
	return out
}

// BenchmarkListModelsCacheHit 测缓存命中时的成本。
//
// 目标：缓存命中不应该遍历号池。当前实现每次 ensure() 都调 activeGroups()，
// 269 个账号 × 每次请求 = 明显的浪费。
func BenchmarkListModelsCacheHit(b *testing.B) {
	accounts := buildAccounts(269)
	fetched := 0
	catalog := NewModelCatalog(&stubLister{accounts: accounts}, func(ctx context.Context, token string) ([]map[string]any, error) {
		fetched++
		return []map[string]any{{"id": "gpt-6"}}, nil
	})
	ctx := context.Background()
	// 预热缓存
	catalog.ListModels(ctx)
	before := fetched

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		catalog.ListModels(ctx)
	}
	b.StopTimer()
	if fetched != before {
		b.Fatalf("cache hit should not refetch: fetched %d -> %d", before, fetched)
	}
}

// BenchmarkListModelsAccountLoop 单独测 activeGroups 的成本。
func BenchmarkListModelsAccountLoop(b *testing.B) {
	accounts := buildAccounts(269)
	catalog := NewModelCatalog(&stubLister{accounts: accounts}, nil)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = catalog.activeGroups()
	}
}
