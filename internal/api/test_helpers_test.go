package api_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/api"
	"github.com/IFuZYI/chatgpt2api/internal/config"
	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/IFuZYI/chatgpt2api/internal/service"
	"github.com/IFuZYI/chatgpt2api/internal/storage"
	"github.com/IFuZYI/chatgpt2api/internal/web"
	"github.com/gin-gonic/gin"
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

	// 任务 goroutine 会在测试结束后继续写 data/image_tasks.json，
	// 而 t.TempDir() 的清理此时会撞上「directory not empty」。
	// 关闭语义是产品代码的一部分（见 ImageTaskService.Close），这里必须用上。
	registerTaskClose(t, deps)
	return deps
}

// registerTaskClose 在测试清理时关闭任务服务，确保没有 goroutine 还在写 data/。
func registerTaskClose(t *testing.T, deps *api.Deps) {
	t.Helper()
	closeTaskService(t, deps.Tasks)
}

// closeTaskService 注册 t.Cleanup 关闭任务服务。
//
// 任务 goroutine 会在测试结束后继续写 data/image_tasks.json，而 t.TempDir()
// 的清理此时会撞上 "directory not empty"。关闭语义是产品代码的一部分
// （见 service.ImageTaskService.Close），测试必须真的用它。
func closeTaskService(t *testing.T, tasks *service.ImageTaskService) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tasks.Close(ctx); err != nil {
			t.Errorf("close image tasks: %v", err)
		}
	})
}

// newTaskTestService 建一个带关闭清理的任务服务（测试自建服务的统一入口）。
func newTaskTestService(t *testing.T) *service.ImageTaskService {
	t.Helper()
	svc := service.NewImageTaskService(newTaskTestConfig(t))
	closeTaskService(t, svc)
	return svc
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

// newTestRouter 构造注入了 Dist 的路由（复刻生产路径）。
//
// 关键差异：golden harness 不注入 Dist，所以 `NoRoute` 走的是 404 分支，
// 而生产环境 Dist 非空、会走 SPA fallback —— 这正是「未知 /api/* 返回
// 200 HTML」这个缺陷在 golden 里测不出来的原因。
//
// Dist 必须指向测试自建的临时前端目录，不能用 web.NewDist() 直接吃编译期
// embed：internal/web/web_dist 被 .gitignore 忽略（仓库里只有 .gitkeep），
// 未构建前端时 embed 里没有任何文件，SPA 断言会全部退化成 404。
func newTestRouter(t *testing.T, deps *api.Deps) *gin.Engine {
	t.Helper()
	if deps.Dist == nil {
		deps.Dist = newTestDist(t)
	}
	return api.NewRouter(deps)
}

// newTestDist 建一个最小可用的前端目录（index.html + 一个 _next 资源），
// 并通过环境变量让 web.NewDist 指向它。
func newTestDist(t *testing.T) *web.Dist {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(testIndexHTML), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	nextDir := filepath.Join(dir, "_next", "static", "chunks")
	if err := os.MkdirAll(nextDir, 0o755); err != nil {
		t.Fatalf("mkdir _next: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nextDir, "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatalf("write _next asset: %v", err)
	}
	t.Setenv("CHATGPT2API_WEB_DIST", dir)
	return web.NewDist()
}

const testIndexHTML = "<!DOCTYPE html><html><body>SPA</body></html>"
