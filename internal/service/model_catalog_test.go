package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeLister 是最小的 AccountLister 实现。
type fakeLister struct{ accounts []Account }

func (f *fakeLister) ListAccounts() []Account { return f.accounts }

func newCatalogWith(accounts []Account, fetch ModelFetcher) *ModelCatalog {
	catalog := NewModelCatalog(&fakeLister{accounts: accounts}, fetch)
	return catalog
}

func TestModelCatalogAggregatesAcrossTypes(t *testing.T) {
	fetch := func(ctx context.Context, token string) ([]map[string]any, error) {
		if token == "" {
			return []map[string]any{{"id": "anon-model", "object": "model"}}, nil
		}
		return []map[string]any{
			{"id": "plus-model", "object": "model"},
			{"id": "anon-model", "object": "model"},
		}, nil
	}
	catalog := newCatalogWith([]Account{
		{"access_token": "t1", "type": "Plus", "status": StatusNormal},
	}, fetch)

	models := catalog.ListModels(context.Background())
	ids := map[string]bool{}
	for _, item := range models {
		ids[stringValue(item["id"])] = true
	}
	if !ids["anon-model"] || !ids["plus-model"] {
		t.Fatalf("union missing models: %v", ids)
	}
	// 去重：anon-model 只应出现一次
	count := 0
	for _, item := range models {
		if stringValue(item["id"]) == "anon-model" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("anon-model appears %d times, want 1", count)
	}
}

// 禁用与异常账号不参与分组（Python 跳过它们）。
func TestModelCatalogSkipsDisabledAccounts(t *testing.T) {
	seen := []string{}
	fetch := func(ctx context.Context, token string) ([]map[string]any, error) {
		seen = append(seen, token)
		return []map[string]any{{"id": "m", "object": "model"}}, nil
	}
	catalog := newCatalogWith([]Account{
		{"access_token": "disabled", "type": "Plus", "status": StatusDisabled},
		{"access_token": "abnormal", "type": "Team", "status": StatusAbnormal},
	}, fetch)

	catalog.ListModels(context.Background())
	for _, token := range seen {
		if token == "disabled" || token == "abnormal" {
			t.Fatalf("disabled/abnormal account was queried: %s", token)
		}
	}
}

// 缓存命中时不重复调用上游。
func TestModelCatalogCaches(t *testing.T) {
	calls := 0
	fetch := func(ctx context.Context, token string) ([]map[string]any, error) {
		calls++
		return []map[string]any{{"id": "m", "object": "model"}}, nil
	}
	catalog := newCatalogWith(nil, fetch)

	catalog.ListModels(context.Background())
	first := calls
	catalog.ListModels(context.Background())
	if calls != first {
		t.Fatalf("cache miss: calls went from %d to %d", first, calls)
	}
}

// 账号集合变化（签名变化）必须立即失效缓存。
func TestModelCatalogInvalidatesOnAccountChange(t *testing.T) {
	calls := 0
	fetch := func(ctx context.Context, token string) ([]map[string]any, error) {
		calls++
		return []map[string]any{{"id": "m", "object": "model"}}, nil
	}
	lister := &fakeLister{}
	catalog := NewModelCatalog(lister, fetch)

	catalog.ListModels(context.Background())
	before := calls

	// 加一个账号，签名变化
	lister.accounts = []Account{{"access_token": "new", "type": "Plus", "status": StatusNormal}}
	catalog.ListModels(context.Background())
	if calls == before {
		t.Fatal("cache should be invalidated when account signature changes")
	}
}

// TTL 过期后重新拉取。
func TestModelCatalogExpires(t *testing.T) {
	calls := 0
	fetch := func(ctx context.Context, token string) ([]map[string]any, error) {
		calls++
		return []map[string]any{{"id": "m", "object": "model"}}, nil
	}
	catalog := newCatalogWith(nil, fetch)

	now := time.Now()
	catalog.clock = func() time.Time { return now }

	catalog.ListModels(context.Background())
	first := calls

	// 未过期
	now = now.Add(100 * time.Second)
	catalog.ListModels(context.Background())
	if calls != first {
		t.Fatalf("should still be cached at +100s")
	}

	// 过期
	now = now.Add(300 * time.Second)
	catalog.ListModels(context.Background())
	if calls == first {
		t.Fatal("should refresh after TTL")
	}
}

// 上游失败时保留旧目录，不清空。
func TestModelCatalogKeepsOldOnFailure(t *testing.T) {
	failing := false
	fetch := func(ctx context.Context, token string) ([]map[string]any, error) {
		if failing {
			return nil, errors.New("upstream down")
		}
		return []map[string]any{{"id": "m", "object": "model"}}, nil
	}
	catalog := newCatalogWith(nil, fetch)

	catalog.ListModels(context.Background())
	failing = true
	catalog.expiresAt = time.Time{} // 强制过期
	models := catalog.ListModels(context.Background())
	if len(models) != 1 || stringValue(models[0]["id"]) != "m" {
		t.Fatalf("old catalog should be retained on failure, got %v", models)
	}
}

func TestModelCatalogRouteForModel(t *testing.T) {
	fetch := func(ctx context.Context, token string) ([]map[string]any, error) {
		if token == "" {
			return []map[string]any{{"id": "public-model"}}, nil
		}
		return []map[string]any{{"id": "plus-model"}}, nil
	}
	catalog := newCatalogWith([]Account{
		{"access_token": "t1", "type": "Plus", "status": StatusNormal},
	}, fetch)

	types, allowAnon := catalog.RouteForModel(context.Background(), "plus-model")
	if !types["Plus"] {
		t.Errorf("plus-model should route to Plus accounts, got %v", types)
	}
	if allowAnon {
		t.Error("plus-model should not allow anonymous")
	}

	types, allowAnon = catalog.RouteForModel(context.Background(), "public-model")
	if len(types) != 0 {
		t.Errorf("public-model should not require accounts, got %v", types)
	}
	if !allowAnon {
		t.Error("public-model should allow anonymous")
	}
}
