package service

import (
	"testing"

	"github.com/basketikun/chatgpt2api/internal/storage"
)

// 备份必须包含账号与密钥快照。
//
// 回归点：BackupBuilder 的 Accounts / Auth 字段在唯一调用点未赋值，
// 于是 `if s.Accounts != nil` 与 `if s.Auth != nil` 恒 false ——
// snapshots/accounts.json 与 auth_keys.json 永远不写入，
// 备份恢复了却没有任何账号和密钥。
func TestBackupBuilderIncludesSnapshots(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-1", "type": "free", "status": StatusNormal, "quota": 25},
	})
	backend, err := storage.NewFromEnv(cfg.DataDir())
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	auth := NewAuthService(backend, cfg.AuthKey)

	settings := BackupSettings{Include: map[string]any{
		"accounts_snapshot":  true,
		"auth_keys_snapshot": true,
	}}

	builder := &BackupBuilder{
		DataDir:  cfg.DataDir(),
		Version:  "2.0.0",
		Accounts: svc,
		Auth:     auth,
	}
	archive, err := builder.Build(settings, "manual")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if _, ok := archive.Snapshots["accounts"]; !ok {
		t.Error("archive has no accounts snapshot — backup would lose all accounts")
	}
	if _, ok := archive.Snapshots["auth_keys"]; !ok {
		t.Error("archive has no auth_keys snapshot — backup would lose all keys")
	}

	// 形状必须是裸数组（Python load_accounts 的返回值），不是 {count,items} 包装。
	if items, ok := archive.Snapshots["accounts"].([]Account); !ok || len(items) == 0 {
		t.Errorf("accounts snapshot = %T, want non-empty []Account", archive.Snapshots["accounts"])
	}
}
