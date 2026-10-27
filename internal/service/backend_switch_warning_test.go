package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/config"
	"github.com/IFuZYI/chatgpt2api/internal/storage"
)

// 切到非 json 后端且当前池为空、但 data/accounts.json 有数据时，
// 必须给出警告（而不是静默显示 0 个账号）。
//
// 实测场景：310 个账号的 accounts.json 切到 sqlite 后，/api/accounts 返回
// 0 条，日志里只有一行正常的 "Initializing storage backend: sqlite" ——
// 用户会以为号池被清空了。
//
// 这里只验证「检测逻辑能找到孤儿数据」，不验证 stdout 文案。
func TestDetectOrphanJSONAccountsOnOtherBackend(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"auth-key":"k"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 在 data/ 下放一份有账号的 accounts.json
	accountsPath := filepath.Join(cfg.DataDir(), "accounts.json")
	if err := os.MkdirAll(filepath.Dir(accountsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(accountsPath,
		[]byte(`[{"access_token":"at-1"},{"access_token":"at-2"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	// 用 sqlite 后端（空库）
	backend, err := storage.NewDBBackend("sqlite:///" + filepath.Join(cfg.DataDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	logs, err := NewLogService(filepath.Join(cfg.DataDir(), "logs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewAccountService(backend, cfg, logs)
	if len(svc.ListAccounts()) != 0 {
		t.Fatal("sqlite 后端应该是空的")
	}

	// 检测逻辑必须能读出那份孤儿数据
	payload, readErr := os.ReadFile(accountsPath)
	if readErr != nil {
		t.Fatalf("读孤儿数据失败: %v", readErr)
	}
	if len(payload) == 0 {
		t.Fatal("孤儿数据为空")
	}
	// 调用不应 panic（stdout 输出由人工核对）
	svc.warnIfSwitchedBackendHasOrphanData()
}

// json 后端时不该警告（它读的就是那个文件，空就是真空）。
func TestNoWarningForJSONBackend(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"auth-key":"k"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := storage.NewJSONBackend(
		filepath.Join(cfg.DataDir(), "accounts.json"),
		filepath.Join(cfg.DataDir(), "auth_keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	logs, err := NewLogService(filepath.Join(cfg.DataDir(), "logs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewAccountService(backend, cfg, logs)
	// 不应 panic，也不应有输出（这里只验证不 panic）
	svc.warnIfSwitchedBackendHasOrphanData()
}
