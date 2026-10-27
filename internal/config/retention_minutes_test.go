package config

import (
	"os"
	"path/filepath"
	"testing"
)

func storeWith(t *testing.T, body string) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return store
}

// 未配置时默认 1440 分钟（= 1 天）。
func TestImageRetentionMinutesDefault(t *testing.T) {
	store := storeWith(t, `{"auth-key":"k"}`)
	if got := store.ImageRetentionMinutes(); got != 1440 {
		t.Errorf("默认值 = %d，应为 1440", got)
	}
}

// **关键**：旧配置的 image_retention_days 必须迁移成分钟。
//
// 回归点：若只读新键、不迁移旧键，`image_retention_days: 30` 会被当成
// 「没有配置」→ 落到默认 1440 分钟 = 1 天 → 用户原本保留 30 天的图
// 在一天内被全部删除。这是静默的数据破坏。
func TestImageRetentionMigratesFromDays(t *testing.T) {
	store := storeWith(t, `{"auth-key":"k","image_retention_days":30}`)
	if got := store.ImageRetentionMinutes(); got != 30*1440 {
		t.Errorf("迁移结果 = %d，应为 30*1440=43200", got)
	}
}

// 新键存在时优先用它，旧键忽略。
func TestImageRetentionMinutesWinsOverDays(t *testing.T) {
	store := storeWith(t, `{"auth-key":"k","image_retention_days":30,"image_retention_minutes":60}`)
	if got := store.ImageRetentionMinutes(); got != 60 {
		t.Errorf("= %d，应为 60（新键优先）", got)
	}
}

// 非法值（0/负数/非数字）回落到默认，不能变成「删掉所有图」。
func TestImageRetentionMinutesRejectsInvalid(t *testing.T) {
	for _, body := range []string{
		`{"auth-key":"k","image_retention_minutes":0}`,
		`{"auth-key":"k","image_retention_minutes":-5}`,
		`{"auth-key":"k","image_retention_minutes":"abc"}`,
		`{"auth-key":"k","image_retention_minutes":""}`,
	} {
		store := storeWith(t, body)
		if got := store.ImageRetentionMinutes(); got != 1440 {
			t.Errorf("%s → %d，应回落 1440", body, got)
		}
	}
}

// 保存后旧键应被清除，避免两个键长期共存造成歧义。
func TestUpdateRemovesLegacyRetentionDays(t *testing.T) {
	store := storeWith(t, `{"auth-key":"k","image_retention_days":30}`)
	if _, err := store.Update(map[string]any{"image_retention_minutes": 2880}); err != nil {
		t.Fatalf("update: %v", err)
	}
	raw := store.Snapshot()
	if _, stale := raw["image_retention_days"]; stale {
		t.Error("image_retention_days 仍在配置里，会与新键歧义")
	}
	if got := store.ImageRetentionMinutes(); got != 2880 {
		t.Errorf("= %d，应为 2880", got)
	}
}

// mode=local 时不应要求 WebDAV URL / 密码 —— 本地模式不用 WebDAV。
//
// 回归点：validateImageStorageSettings 只看 enabled，不看 mode。
// 于是 `enabled:true, mode:"local"` 这种完全合法的配置无法通过 Update
// 保存（实测：newImageStorageFixture 造出来的配置就是这样）。
func TestImageStorageValidationSkipsWebdavFieldsInLocalMode(t *testing.T) {
	store := storeWith(t, `{"auth-key":"k"}`)
	_, err := store.Update(map[string]any{
		"image_storage": map[string]any{
			"enabled":          true,
			"mode":             "local",
			"webdav_url":       "",
			"webdav_password":  "",
			"webdav_root_path": "chatgpt2api/images",
		},
	})
	if err != nil {
		t.Errorf("mode=local 不该要求 WebDAV 字段，实际报错: %v", err)
	}
}

// 但 mode=webdav 时仍然必须校验。
func TestImageStorageValidationStillRequiresWebdavInWebdavMode(t *testing.T) {
	store := storeWith(t, `{"auth-key":"k"}`)
	_, err := store.Update(map[string]any{
		"image_storage": map[string]any{
			"enabled":    true,
			"mode":       "webdav",
			"webdav_url": "",
		},
	})
	if err == nil {
		t.Error("mode=webdav 且 URL 为空时应报错")
	}
}
