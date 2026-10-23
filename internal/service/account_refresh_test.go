package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/storage"
)

// newTestAccountService 建一个临时号池。
func newTestAccountService(t *testing.T, accounts []map[string]any) (*AccountService, *config.Store) {
	t.Helper()
	dir := t.TempDir()
	cfgJSON := []byte(`{"auth-key":"test"}`)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cfgJSON, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("2.0.0"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	t.Setenv("CHATGPT2API_AUTH_KEY", "")
	t.Setenv("STORAGE_BACKEND", "json")

	cfg, err := config.NewStore(dir)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	backend, err := storage.NewFromEnv(cfg.DataDir())
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	if len(accounts) > 0 {
		if err := backend.SaveAccounts(accounts); err != nil {
			t.Fatalf("seed accounts: %v", err)
		}
	}
	logs, err := NewLogService(filepath.Join(cfg.DataDir(), "logs.jsonl"))
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	return NewAccountService(backend, cfg, logs), cfg
}

// 刷新必须把拿到的 email/quota/type 写回号池。
// 只计数不回写会让刷新「成功」但数据永远是空（实测踩到）。
func TestRefreshAccountsWritesBack(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-1", "type": "free", "status": "正常", "quota": 0},
	})

	verify := func(ctx context.Context, token, event string) (Account, error) {
		return Account{
			"access_token": "tok-1",
			"email":        "user@example.com",
			"user_id":      "user-abc",
			"type":         "free",
			"quota":        24,
			"status":       "正常",
			"restore_at":   "2026-10-24T07:34:45+00:00",
		}, nil
	}

	result := svc.RefreshAccounts(context.Background(), []string{"tok-1"}, "", verify)
	if result["refreshed"] != 1 {
		t.Fatalf("refreshed = %v, want 1", result["refreshed"])
	}

	account := svc.GetAccount("tok-1")
	if account == nil {
		t.Fatal("account disappeared")
	}
	if account["email"] != "user@example.com" {
		t.Errorf("email not written back: %v", account["email"])
	}
	if account["quota"] != 24 {
		t.Errorf("quota not written back: %v", account["quota"])
	}
	if account["restore_at"] != "2026-10-24T07:34:45+00:00" {
		t.Errorf("restore_at not written back: %v", account["restore_at"])
	}
}

// 刷新失败要记录 last_refresh_error，且不改动其他字段。
func TestRefreshAccountsRecordsError(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-1", "type": "free", "status": "正常", "quota": 5, "email": "keep@example.com"},
	})

	verify := func(ctx context.Context, token, event string) (Account, error) {
		return nil, context.DeadlineExceeded
	}

	result := svc.RefreshAccounts(context.Background(), []string{"tok-1"}, "", verify)
	if result["refreshed"] != 0 {
		t.Errorf("refreshed = %v, want 0", result["refreshed"])
	}
	errorsOut, _ := result["errors"].([]any)
	if len(errorsOut) != 1 {
		t.Fatalf("errors = %v", errorsOut)
	}

	account := svc.GetAccount("tok-1")
	if account == nil {
		t.Fatal("account should still exist after refresh failure")
	}
	if account["last_refresh_error"] == nil {
		t.Error("last_refresh_error should be recorded")
	}
	// 失败不应清掉原有数据
	if account["email"] != "keep@example.com" {
		t.Errorf("email should be preserved on failure: %v", account["email"])
	}
	if account["quota"] != 5 {
		t.Errorf("quota should be preserved on failure: %v", account["quota"])
	}
}

// 刷新时 token 轮换：旧 token 要能通过别名解析到新 token。
func TestRefreshAccountsHandlesTokenRotation(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "old-token", "type": "free", "status": "正常", "quota": 3},
	})

	verify := func(ctx context.Context, token, event string) (Account, error) {
		return Account{
			"access_token": "new-token",
			"email":        "rotated@example.com",
			"type":         "free",
			"quota":        10,
			"status":       "正常",
		}, nil
	}

	svc.RefreshAccounts(context.Background(), []string{"old-token"}, "", verify)

	// 旧 token 应解析到新 token
	if resolved := svc.ResolveAccessToken("old-token"); resolved != "new-token" {
		t.Errorf("alias resolution = %q, want new-token", resolved)
	}
	// 新 token 存在且数据已更新
	account := svc.GetAccount("new-token")
	if account == nil {
		t.Fatal("new token account missing")
	}
	if account["email"] != "rotated@example.com" || account["quota"] != 10 {
		t.Errorf("rotated account not updated: %v", account)
	}
	// 旧 token 条目应被删除（不是留着两份）
	svc.mu.RLock()
	_, oldExists := svc.accounts["old-token"]
	svc.mu.RUnlock()
	if oldExists {
		t.Error("old token entry should be removed after rotation")
	}
}

// 刷新成功后要清掉上一次的错误。
func TestRefreshClearsPreviousError(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{
			"access_token": "tok-1", "type": "free", "status": "正常",
			"last_refresh_error": "old error", "last_refresh_error_at": "2026-10-01 00:00:00",
		},
	})

	verify := func(ctx context.Context, token, event string) (Account, error) {
		return Account{"access_token": "tok-1", "email": "ok@example.com", "type": "free", "quota": 1, "status": "正常"}, nil
	}
	svc.RefreshAccounts(context.Background(), []string{"tok-1"}, "", verify)

	account := svc.GetAccount("tok-1")
	if account["last_refresh_error"] != nil {
		t.Errorf("previous error should be cleared: %v", account["last_refresh_error"])
	}
}

// 上游刷新成功后本地维护的字段不能被覆盖掉。
func TestRefreshPreservesLocalFields(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{
			"access_token": "tok-1", "type": "free", "status": "正常",
			"proxy": "socks5h://local:1080", "success": 7, "fail": 2,
			"created_at": "2026-01-01 00:00:00",
		},
	})

	verify := func(ctx context.Context, token, event string) (Account, error) {
		return Account{"access_token": "tok-1", "email": "a@b.c", "type": "free", "quota": 9, "status": "正常"}, nil
	}
	svc.RefreshAccounts(context.Background(), []string{"tok-1"}, "", verify)

	account := svc.GetAccount("tok-1")
	if account["proxy"] != "socks5h://local:1080" {
		t.Errorf("proxy lost: %v", account["proxy"])
	}
	if account["success"] != 7 || account["fail"] != 2 {
		t.Errorf("counters lost: success=%v fail=%v", account["success"], account["fail"])
	}
	if account["created_at"] != "2026-01-01 00:00:00" {
		t.Errorf("created_at lost: %v", account["created_at"])
	}
}
