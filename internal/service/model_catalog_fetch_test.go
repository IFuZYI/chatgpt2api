package service

import (
	"context"
	"errors"
	"testing"
)

// 传 nil fetcher 时目录为空 —— 这是「静默失败」的根源：
// /v1/models 不会报错，只是少了全部文本模型。
// 这个测试固定住这个行为，让「忘记注入 fetcher」在评审时可见。
func TestModelCatalogNilFetcherYieldsEmpty(t *testing.T) {
	svc := &stubAccountLister{accounts: []Account{
		{"access_token": "t1", "type": "free", "status": StatusNormal},
	}}
	catalog := NewModelCatalog(svc, nil)
	models := catalog.ListModels(context.Background())
	if len(models) != 0 {
		t.Errorf("nil fetcher should yield 0 models, got %d", len(models))
	}
}

// 注入 fetcher 后必须能拿到文本模型。
func TestModelCatalogFetchProvidesTextModels(t *testing.T) {
	svc := &stubAccountLister{accounts: []Account{
		{"access_token": "t1", "type": "free", "status": StatusNormal},
	}}
	calls := 0
	fetch := func(ctx context.Context, accessToken string) ([]map[string]any, error) {
		calls++
		return []map[string]any{
			{"id": "gpt-6", "object": "model", "root": "gpt-6"},
			{"id": "gpt-6-mini", "object": "model", "root": "gpt-6-mini"},
		}, nil
	}
	catalog := NewModelCatalog(svc, fetch)
	models := catalog.ListModels(context.Background())
	if len(models) == 0 {
		t.Fatal("fetcher provided models but catalog is empty")
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[stringValue(m["id"])] = true
	}
	if !ids["gpt-6"] {
		t.Errorf("gpt-6 missing from catalog: %v", ids)
	}
	if calls == 0 {
		t.Error("fetcher was never called")
	}
}

// 上游失败时要保留旧目录，不能把目录清空（抖动时模型会忽有忽无）。
func TestModelCatalogKeepsPreviousOnFetchError(t *testing.T) {
	svc := &stubAccountLister{accounts: []Account{
		{"access_token": "t1", "type": "free", "status": StatusNormal},
	}}
	fail := false
	fetch := func(ctx context.Context, accessToken string) ([]map[string]any, error) {
		if fail {
			return nil, errors.New("upstream down")
		}
		return []map[string]any{{"id": "gpt-6"}}, nil
	}
	catalog := NewModelCatalog(svc, fetch)

	first := catalog.ListModels(context.Background())
	if len(first) == 0 {
		t.Fatal("first fetch should succeed")
	}

	// 强制过期，再让上游失败
	catalog.expiresAt = catalog.clock().Add(-1)
	catalog.signature = "" // 触发刷新
	fail = true
	second := catalog.ListModels(context.Background())
	if len(second) == 0 {
		t.Error("catalog should keep previous models when upstream fails")
	}
}

// stubAccountLister 是最小号池替身。
type stubAccountLister struct{ accounts []Account }

func (s *stubAccountLister) ListAccounts() []Account { return s.accounts }
