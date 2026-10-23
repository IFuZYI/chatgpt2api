package api_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/api"
	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/basketikun/chatgpt2api/internal/storage"
)

// newTestDeps 构造一套最小依赖（临时目录 + JSON 存储）。
func newTestDeps(t *testing.T) *api.Deps {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auth-key":"test"}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("2.0.0"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}

	cfg, err := config.NewStore(dir)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	backend, err := storage.NewFromEnv(cfg.DataDir())
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	logs, err := service.NewLogService(filepath.Join(cfg.DataDir(), "logs.jsonl"))
	if err != nil {
		t.Fatalf("logs: %v", err)
	}

	deps := &api.Deps{
		Config:  cfg,
		Storage: backend,
		Logs:    logs,
		Auth:    service.NewAuthService(backend, cfg.AuthKey),
		Accts:   service.NewAccountService(backend, cfg, logs),
		Images:  service.NewImageService(cfg),
		Tasks:   service.NewImageTaskService(cfg),
		CPA:     service.NewCPAConfig(cfg.DataDir()),
		Sub2API: service.NewSub2APIConfig(cfg.DataDir()),
		OAuth:   service.NewOAuthLoginService(),
	}
	deps.Models = service.NewModelCatalog(deps.Accts, nil)
	deps.ChatCache = protocol.NewChatCompletionCache(deps.ChatCacheSettings)
	return deps
}

// newTaskTestConfig 给 service 层测试用的最小配置。
func newTaskTestConfig(t *testing.T) interface {
	DataDir() string
	ImageRetentionDays() int
} {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auth-key":"test"}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("2.0.0"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	cfg, err := config.NewStore(dir)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func stringValueOfTest(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func sleepShort() { time.Sleep(20 * time.Millisecond) }

func timeoutChan() <-chan time.Time { return time.After(3 * time.Second) }
