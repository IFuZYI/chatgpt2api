package service

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/config"
	"github.com/IFuZYI/chatgpt2api/internal/storage"
)

// newWarningFixture 造一个「非 json 后端 + 空库 + data/accounts.json 有账号」的场景。
func newWarningFixture(t *testing.T, orphanAccounts string) (*AccountService, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"auth-key":"k"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if orphanAccounts != "" {
		accountsPath := filepath.Join(cfg.DataDir(), "accounts.json")
		if err := os.MkdirAll(filepath.Dir(accountsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(accountsPath, []byte(orphanAccounts), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	backend, err := storage.NewDBBackend("sqlite:///" + filepath.Join(cfg.DataDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	logs, err := NewLogService(filepath.Join(cfg.DataDir(), "logs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	svc := NewAccountService(backend, cfg, logs)
	svc.warnOut = buf
	return svc, buf
}

// 非 json 后端 + 空池 + data/accounts.json 有账号 → 必须告警，且内容要点齐全。
//
// 回归点：早期这个测试只调用函数、不断言输出（注释写「stdout 由人工核对」），
// 所以它连「告警根本没打印」都发现不了。
func TestOrphanAccountsTriggersWarning(t *testing.T) {
	svc, buf := newWarningFixture(t, `[{"access_token":"at-1"},{"access_token":"at-2"},{"access_token":"at-3"}]`)

	svc.warnIfSwitchedBackendHasOrphanData()

	got := buf.String()
	if got == "" {
		t.Fatal("检测到孤儿账号但没有输出任何告警")
	}
	for _, must := range []string{
		"WARNING",
		"accounts.json",
		"3 个账号",              // 条数要对
		"不会自动迁移数据",           // 说明原因
		"POST /api/accounts", // 给出迁移方式
	} {
		if !strings.Contains(got, must) {
			t.Errorf("告警缺少关键信息 %q\n实际输出:\n%s", must, got)
		}
	}
}

// 池子非空时不该告警（否则每次正常启动都刷警告）。
func TestNoWarningWhenPoolNotEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auth-key":"k"}`), 0o644); err != nil {
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
	buf := &bytes.Buffer{}
	svc := NewAccountService(backend, cfg, logs)
	svc.warnOut = buf
	svc.AddAccountItems([]map[string]any{
		{"access_token": "at-x", "type": "free", "status": StatusNormal, "quota": 1},
	}, nil)
	buf.Reset() // 忽略 AddAccountItems 期间的输出

	svc.warnIfSwitchedBackendHasOrphanData()
	if buf.String() != "" {
		t.Errorf("池子非空却打了告警:\n%s", buf.String())
	}
}

// json 后端不告警：它读的就是那个文件，空就是真空。
func TestNoWarningForJSONBackend(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auth-key":"k"}`), 0o644); err != nil {
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
	buf := &bytes.Buffer{}
	svc := NewAccountService(backend, cfg, logs)
	svc.warnOut = buf

	svc.warnIfSwitchedBackendHasOrphanData()
	if buf.String() != "" {
		t.Errorf("json 后端不该告警（它读的就是那个文件）:\n%s", buf.String())
	}
}
