package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// 凭据文件必须是 0600，不能是 0644。
//
// accounts.json 含 access_token / refresh_token / password / totp_secret /
// session_token —— 0644 意味着同机任何用户可读。auth_keys.json 更是直接
// 拿到 admin bearer。
func TestJSONStoreWritesCredentialsAs0600(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONBackend(filepath.Join(dir, "accounts.json"), filepath.Join(dir, "auth_keys.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.SaveAccounts([]map[string]any{
		{"access_token": "at-1", "password": "pw"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("accounts.json 权限是 %04o，应为 0600（含凭据，不能组/其他可读）", mode)
	}
}
