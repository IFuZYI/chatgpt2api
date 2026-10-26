package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 已设置过 WebDAV 密码时，只改其他设置不能报「必须填写密码」。
//
// 回归点：v2.2.0 的脱敏把 webdav_password 换成 has_webdav_password，
// 前端拿不到原文 → 回传空串 → validateImageStorageSettings 判为空 →
// 报错。用户明明早就配好了密码。
//
// 正确语义：收到 has_webdav_password=true 且 webdav_password 为空时，
// 保留磁盘上已有的密码。
func TestUpdateKeepsExistingWebdavPassword(t *testing.T) {
	dir := t.TempDir()
	seed := `{
		"auth-key":"k",
		"image_storage":{
			"enabled":true,
			"mode":"webdav",
			"webdav_url":"https://dav.example.com",
			"webdav_username":"u",
			"webdav_password":"EXISTING-SECRET"
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	// 模拟前端：API 层（maskConfigSecrets）把 webdav_password 换成
	// has_webdav_password，前端拿到的是这个形态。
	// 注意 Get() 本身不脱敏 —— 脱敏在 api 层做。
	raw := store.ImageStorageSettings()
	if got, _ := raw["webdav_password"].(string); got != "EXISTING-SECRET" {
		t.Fatalf("前提不成立：内部访问器应返回原文，实际 %q", got)
	}

	// 前端原样回传（密码留空、只带 has_*），同时改一个无关字段
	patch := map[string]any{
		"image_storage": map[string]any{
			"enabled":             true,
			"mode":                "webdav",
			"webdav_url":          "https://dav.example.com",
			"webdav_username":     "u",
			"webdav_password":     "",
			"has_webdav_password": true,
			"webdav_root_path":    "chatgpt2api/images",
		},
		"image_retention_days": 7,
	}
	if _, err := store.Update(patch); err != nil {
		t.Fatalf("只改其他设置却报错: %v", err)
	}

	// 密码必须还在
	after := store.ImageStorageSettings()
	if got := after["webdav_password"]; got != "EXISTING-SECRET" {
		t.Errorf("密码丢失：%v（应保留 EXISTING-SECRET）", got)
	}
}

// 真正改成空密码时仍要拦（不能因为兼容 has_* 就放行空密码）。
func TestUpdateRejectsWebdavWithoutAnyPassword(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auth-key":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	// 全新配置：没有旧密码、也没有 has_* 标记 → 必须报错
	_, updateErr := store.Update(map[string]any{
		"image_storage": map[string]any{
			"enabled":         true,
			"mode":            "webdav",
			"webdav_url":      "https://dav.example.com",
			"webdav_username": "u",
			"webdav_password": "",
		},
	})
	if updateErr == nil {
		t.Error("没有任何密码时不该放行")
	}
}
