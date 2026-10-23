package api_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/basketikun/chatgpt2api/internal/api"
	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/basketikun/chatgpt2api/internal/storage"
)

// TestRouteParity 断言 Go 版注册的路由集合覆盖 Python 版的全部 78 个 method。
//
// 路由缺失在生产里表现为某个前端页面静默失效，而 golden 只覆盖被点名的用例。
// 这个测试直接对着 Python 的路由表比，是覆盖面的兜底。
func TestRouteParity(t *testing.T) {
	dir := t.TempDir()
	raw := map[string]any{"auth-key": "chatgpt2api"}
	encoded, _ := json.Marshal(raw)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), encoded, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("1.8.0"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	t.Setenv("CHATGPT2API_AUTH_KEY", "")
	t.Setenv("STORAGE_BACKEND", "json")

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
		Proxy:   service.NewProxySettings(cfg),
		Images:  service.NewImageService(cfg),
		Tasks:   service.NewImageTaskService(cfg),
	}
	registerTaskClose(t, deps)
	engine := api.NewRouter(deps)

	registered := map[string]bool{}
	for _, route := range engine.Routes() {
		registered[route.Method+" "+normalizeGinPath(route.Path)] = true
	}

	// Python 版的路由表（api/app.py + 四个 router 的实测输出）。
	// 格式 "METHOD /path"，路径参数统一写成 {x}。
	required := []string{
		"GET /version",
		"GET /health",
		"POST /auth/login",

		"GET /api/settings",
		"POST /api/settings",

		"GET /api/auth/users",
		"POST /api/auth/users",
		"POST /api/auth/users/{x}",
		"DELETE /api/auth/users/{x}",

		"GET /api/accounts",
		"POST /api/accounts",
		"DELETE /api/accounts",
		"POST /api/accounts/update",
		"POST /api/accounts/export",
		"POST /api/accounts/refresh",
		"GET /api/accounts/refresh/progress/{x}",
		"POST /api/accounts/re-login",
		"GET /api/accounts/re-login/progress/{x}",
		"POST /api/accounts/oauth/start",
		"POST /api/accounts/oauth/finish",

		"GET /api/cpa/pools",
		"POST /api/cpa/pools",
		"POST /api/cpa/pools/{x}",
		"DELETE /api/cpa/pools/{x}",
		"GET /api/cpa/pools/{x}/files",
		"GET /api/cpa/pools/{x}/import",
		"POST /api/cpa/pools/{x}/import",

		"GET /api/sub2api/servers",
		"POST /api/sub2api/servers",
		"POST /api/sub2api/servers/{x}",
		"DELETE /api/sub2api/servers/{x}",
		"GET /api/sub2api/servers/{x}/groups",
		"GET /api/sub2api/servers/{x}/accounts",
		"GET /api/sub2api/servers/{x}/import",
		"POST /api/sub2api/servers/{x}/import",

		"GET /api/images",
		"POST /api/images/delete",
		"POST /api/images/download",
		"GET /api/images/download/{x}",
		"GET /api/images/storage",
		"POST /api/images/storage/compress",
		"POST /api/images/storage/cleanup-to-target",
		"GET /api/images/tags",
		"POST /api/images/tags",
		"DELETE /api/images/tags/{x}",
		"GET /images/{x}",
		"GET /image-thumbnails/{x}",

		"GET /api/logs",
		"POST /api/logs/delete",

		"POST /api/proxy/test",
		"GET /api/proxy/runtime",
		"POST /api/proxy/runtime",
		"POST /api/proxy/clearance/test",

		"GET /api/storage/info",
		"POST /api/backup/test",
		"GET /api/backups",
		"POST /api/backups/run",
		"POST /api/backups/delete",
		"GET /api/backups/detail",
		"GET /api/backups/download",
		"POST /api/image-storage/test",
		"POST /api/image-storage/sync",

		"GET /api/image-tasks",
		"POST /api/image-tasks/generations",
		"POST /api/image-tasks/edits",
		"POST /api/image-tasks/{x}/resume-poll",

		"GET /v1/models",
		"POST /v1/images/generations",
		"POST /v1/images/edits",
		"POST /v1/chat/completions",
		"POST /v1/responses",
		"POST /v1/messages",
		"POST /v1/search",
		"GET /v1/editable-file-tasks",
		"GET /files/{x}",
		"POST /v1/ppt/generations",
		"POST /v1/psd/generations",
	}

	missing := make([]string, 0)
	for _, route := range required {
		if !registered[route] {
			missing = append(missing, route)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Fatalf("%d/%d routes missing:\n  %s",
			len(missing), len(required), joinLines(missing))
	}
	t.Logf("all %d routes registered (%d total gin routes)", len(required), len(registered))
}

// normalizeGinPath 把 Gin 的 :param 与 *param 统一成 {x}。
func normalizeGinPath(path string) string {
	out := make([]byte, 0, len(path))
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case ':':
			out = append(out, '{', 'x', '}')
			for i+1 < len(path) && path[i+1] != '/' {
				i++
			}
		case '*':
			out = append(out, '{', 'x', '}')
			for i+1 < len(path) && path[i+1] != '/' {
				i++
			}
		default:
			out = append(out, path[i])
		}
	}
	return string(out)
}

func joinLines(items []string) string {
	out := ""
	for index, item := range items {
		if index > 0 {
			out += "\n  "
		}
		out += item
	}
	return out
}
