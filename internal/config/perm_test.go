package config

import (
	"os"
	"path/filepath"
	"testing"
)

// config.json 含 auth-key（admin bearer）、R2 secret、备份口令、代理密码。
// 必须 0600。
func TestConfigFileIsWrittenAs0600(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auth-key":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := store.Update(map[string]any{"image_retention_days": 3}); err != nil {
		t.Fatalf("update: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("config.json 权限是 %04o，应为 0600（含 auth-key 等秘密）", mode)
	}
}
