package config

import (
	"os"
	"path/filepath"
	"testing"
)

func newStoreWithSecrets(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	seed := `{
		"auth-key":"k",
		"backup":{"enabled":true,"provider":"cloudflare_r2","bucket":"b",
		          "access_key_id":"AK","secret_access_key":"R2-SECRET",
		          "passphrase":"BACKUP-PASS","interval_minutes":1440},
		"ai_review":{"enabled":true,"base_url":"https://api","api_key":"sk-SECRET","model":"m"}
	}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return store
}

// 保存设置不能抹掉 R2 密钥与备份口令。
//
// 回归点：前端 saveConfig 每次全量提交所有 section，而脱敏后
// secret_access_key / passphrase 在响应里不存在 → 前端回传空串 →
// Update 只做 normalize（不保留旧值）→ 密钥被静默清空。
//
// 这比 WebDAV 那条更难发现：没有校验错误，只是某天备份突然连不上。
func TestUpdatePreservesBackupSecrets(t *testing.T) {
	store := newStoreWithSecrets(t)

	// 模拟前端：只改一个无关字段，凭据留空并带 has_* 标记
	_, err := store.Update(map[string]any{
		"backup": map[string]any{
			"enabled":               true,
			"provider":              "cloudflare_r2",
			"bucket":                "b",
			"access_key_id":         "AK",
			"secret_access_key":     "",
			"passphrase":            "",
			"has_secret_access_key": true,
			"has_passphrase":        true,
			"interval_minutes":      1440,
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	settings := store.BackupSettings()
	if got := settings["secret_access_key"]; got != "R2-SECRET" {
		t.Errorf("R2 密钥被抹掉：%v（应保留 R2-SECRET）", got)
	}
	if got := settings["passphrase"]; got != "BACKUP-PASS" {
		t.Errorf("备份口令被抹掉：%v（应保留 BACKUP-PASS）", got)
	}
}

// 保存设置不能抹掉 AI 审核的 api_key。
func TestUpdatePreservesAIReviewKey(t *testing.T) {
	store := newStoreWithSecrets(t)

	_, err := store.Update(map[string]any{
		"ai_review": map[string]any{
			"enabled":     true,
			"base_url":    "https://api",
			"api_key":     "",
			"has_api_key": true,
			"model":       "m",
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	if got := store.AIReview()["api_key"]; got != "sk-SECRET" {
		t.Errorf("AI key 被抹掉：%v（应保留 sk-SECRET）", got)
	}
}

// has_* 不能落进配置文件。
func TestHasFlagsAreNotPersisted(t *testing.T) {
	store := newStoreWithSecrets(t)
	_, err := store.Update(map[string]any{
		"backup":    map[string]any{"enabled": true, "has_secret_access_key": true, "has_passphrase": true},
		"ai_review": map[string]any{"enabled": true, "has_api_key": true},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	settings := store.BackupSettings()
	for key := range settings {
		if len(key) > 4 && key[:4] == "has_" {
			t.Errorf("backup 里混入 %q", key)
		}
	}
	for key := range store.AIReview() {
		if len(key) > 4 && key[:4] == "has_" {
			t.Errorf("ai_review 里混入 %q", key)
		}
	}
}
