package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// 损坏的 accounts.json 必须报错，不能静默当成空池。
//
// 真实数据丢失路径：
//  1. accounts.json 被截断/手改错一个字符
//  2. LoadAccounts 返回 ([], nil) —— 「损坏」与「空」不可区分
//  3. service 层把错误当空池，下一次 saveAccountsLocked() 写入空数组
//  4. 整个号池被永久清空，无备份
//
// 正确行为：文件存在但解析失败 → 返回错误，让调用方拒绝覆盖。
func TestLoadAccountsReportsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(path, []byte(`[{"access_token":"at-1"`), 0o600); err != nil {
		t.Fatal(err)
	}

	backend, err := NewJSONBackend(path, filepath.Join(dir, "auth_keys.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	items, loadErr := backend.LoadAccounts()
	if loadErr == nil {
		t.Fatalf("损坏的文件没有报错（返回 %d 条）—— 调用方会把它当空池写回", len(items))
	}
}

// 文件不存在是正常情况（首次启动），应返回空列表 + nil 错误。
func TestLoadAccountsMissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	backend, err := NewJSONBackend(filepath.Join(dir, "accounts.json"), filepath.Join(dir, "auth_keys.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	items, loadErr := backend.LoadAccounts()
	if loadErr != nil {
		t.Errorf("文件不存在不该报错: %v", loadErr)
	}
	if len(items) != 0 {
		t.Errorf("应为空列表，实际 %d 条", len(items))
	}
}
