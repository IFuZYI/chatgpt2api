package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/config"
	"github.com/IFuZYI/chatgpt2api/internal/storage"
)

// 账号池文件损坏时，保存必须被拒绝 —— 不能把空池覆盖到磁盘。
//
// 真实数据丢失路径：accounts.json 被截断 → 加载失败 → 服务把错误当空池
// → 下一次保存写入空数组 → 全池 token 永久丢失。
func TestCorruptPoolFileBlocksSaves(t *testing.T) {
	dir := t.TempDir()
	accountsPath := filepath.Join(dir, "data", "accounts.json")
	if err := os.MkdirAll(filepath.Dir(accountsPath), 0o755); err != nil {
		t.Fatal(err)
	}

	// 写一个损坏的文件
	corrupt := []byte(`[{"access_token":"at-survive","email":"s@x.test"`)
	if err := os.WriteFile(accountsPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	// 按 helper 的方式手工装配，但目录由本测试控制
	t.Setenv("CHATGPT2API_AUTH_KEY", "")
	t.Setenv("STORAGE_BACKEND", "json")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auth-key":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, cfgErr := config.NewStore(dir)
	if cfgErr != nil {
		t.Fatalf("config: %v", cfgErr)
	}
	backend, backErr := storage.NewFromEnv(cfg.DataDir())
	if backErr != nil {
		t.Fatalf("storage: %v", backErr)
	}
	logs, logErr := NewLogService(filepath.Join(cfg.DataDir(), "logs.jsonl"))
	if logErr != nil {
		t.Fatalf("logs: %v", logErr)
	}
	svc := NewAccountService(backend, cfg, logs)

	// 触发一次写操作（加账号会调用 saveAccountsLocked）
	svc.AddAccounts([]string{"at-new"}, "")

	// 磁盘文件必须保持原样（损坏但不被空池覆盖）
	after, err := os.ReadFile(accountsPath)
	if err != nil {
		t.Fatalf("文件消失了: %v", err)
	}
	if string(after) != string(corrupt) {
		t.Errorf("损坏的文件被覆盖了 —— 原 %d 字节，现在 %d 字节：%q",
			len(corrupt), len(after), string(after[:min(80, len(after))]))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
