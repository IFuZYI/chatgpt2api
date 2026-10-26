package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Get() 必须返回深拷贝：调用方改返回值不能污染内部状态。
//
// 回归点：Snapshot() 只做 `out[key] = value` 的浅拷贝，嵌套 map/slice
// 仍与 s.data 共享 —— 调用方（如 GET /api/settings 的序列化后处理、
// 或任何写返回值的代码）改了嵌套字段，就绕过了 mu 直接改内部状态，
// 与并发读者竞争。
func TestGetReturnsDeepCopyOfNestedMaps(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{
		"auth-key":"k",
		"backup":{"enabled":true,"passphrase":"secret"},
		"image_storage":{"provider":"local"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	first := store.Get()
	backup, ok := first["backup"].(map[string]any)
	if !ok {
		t.Fatalf("backup 不是 map: %T", first["backup"])
	}
	// 篡改返回值里的嵌套字段
	backup["passphrase"] = "TAMPERED"

	second := store.Get()
	backup2, _ := second["backup"].(map[string]any)
	if got := backup2["passphrase"]; got == "TAMPERED" {
		t.Error("Get() 返回的是共享引用 —— 改返回值污染了内部状态")
	}
}

// AIReview() 同理，必须返回副本。
func TestAIReviewReturnsCopy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{
		"auth-key":"k",
		"ai_review":{"enabled":true,"model":"gpt-5-mini"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	live := store.AIReview()
	live["model"] = "TAMPERED"

	if got := store.AIReview()["model"]; got == "TAMPERED" {
		t.Error("AIReview() 返回共享 map —— 改返回值污染了内部状态")
	}
}
