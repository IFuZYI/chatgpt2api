package api_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/api"
	"github.com/IFuZYI/chatgpt2api/internal/web"
)

// 未知的 /api/* 与 /v1/* 必须返回 404 JSON，不能被 SPA fallback 吞成 200 HTML。
//
// 回归点：NoRoute 对所有 GET 都走 spa()，于是拼错路径的客户端拿到
// 200 + 一段 HTML 而不是 JSON 错误。OpenAI SDK 会把 HTML 当成功响应解析失败。
// FastAPI 对未知 API 路径返回 404 {"detail":"Not Found"}。
//
// 刻意不使用 `/api/third-party-apps` 这类「Python 有、Go 没有」的真实路径：
// 那会把「路由缺失」固化成期望行为。这里只断言「未注册的 API 路径」的形态。
func TestUnknownAPIPathsReturnJSON404(t *testing.T) {
	deps := newTestDeps(t)
	router := newTestRouter(t, deps)

	cases := []string{
		"/api/nonexistent-endpoint",
		"/api/nonexistent-endpoint/nested",
		"/v1/nonexistent-endpoint",
		"/v1/messages/extra",
		// 前缀根：`/api` `/v1` 没有对应路由，也不能落到 SPA。
		"/api",
		"/v1",
	}
	for _, path := range cases {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer test")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		contentType := rec.Header().Get("Content-Type")
		if !strings.Contains(contentType, "application/json") {
			t.Errorf("GET %s content-type = %q, want application/json (got HTML from SPA fallback)",
				path, contentType)
		}
		// 三套信封按前缀分流：/api/* 用 {"detail":...}，/v1/* 用 {"error":{...}}。
		// 两者都是合法 JSON 错误，只要不是 HTML 就算通过。
		body := rec.Body.String()
		if !strings.Contains(body, "detail") && !strings.Contains(body, "error") {
			t.Errorf("GET %s body = %q, want a JSON error envelope (detail or error field)", path, body)
		}
	}
}

// 真正的 SPA 路由必须仍然返回 HTML（不能因为上次修复而破坏前端）。
//
// 清单来自 web/src/app/ 的实际页面目录：isAPIPath 的前缀表不能吞掉任何一个。
func TestSPARoutesStillServeHTML(t *testing.T) {
	deps := newTestDeps(t)
	router := newTestRouter(t, deps)

	// 未构建前端时 web_dist 里只有 .gitkeep，spa() 会返回 404 —— 此时
	// 本测试没有意义（404 也是 text/html）。跳过而不是让断言变弱。
	if !webDistBuilt() {
		t.Skip("web_dist not built; run `npm run build` in web/ to exercise the SPA fallback")
	}

	paths := []string{
		"/",
		"/accounts",
		"/debug",
		"/image",
		"/image-manager",
		"/login",
		"/logs",
		"/settings",
	}
	for _, path := range paths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (SPA fallback must serve the shell)", path, rec.Code)
		}
		contentType := rec.Header().Get("Content-Type")
		if !strings.Contains(contentType, "text/html") {
			t.Errorf("GET %s content-type = %q, want text/html", path, contentType)
		}
	}
}

// webDistBuilt 报告前端产物是否真的构建过（而非只有 .gitkeep 占位）。
func webDistBuilt() bool {
	entries, err := os.ReadDir(filepath.Join("..", "web", "web_dist"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Name() != ".gitkeep" {
			return true
		}
	}
	return false
}

// 未注册的 /_next/ 资源仍必须 404（不能回退到 SPA）。
func TestMissingNextAssetReturns404(t *testing.T) {
	deps := newTestDeps(t)
	router := newTestRouter(t, deps)

	req := httptest.NewRequest(http.MethodGet, "/_next/static/chunks/missing.js", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("missing _next asset = %d, want 404", rec.Code)
	}
}

// 已存在的 _next 资源必须照常返回（isAPIPath 不得误伤静态资源）。
func TestExistingNextAssetServed(t *testing.T) {
	deps := newTestDeps(t)
	router := newTestRouter(t, deps)

	req := httptest.NewRequest(http.MethodGet, "/_next/static/chunks/app.js", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("existing _next asset = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "console.log(1)" {
		t.Errorf("body = %q", body)
	}
}

// 没有前端产物时（Dist 指向空目录），未知的非 API 路径也必须是 404 JSON，
// 而不是 200 或空 body。
func TestUnknownNonAPIPathFallsBackTo404WhenNoDist(t *testing.T) {
	deps := newTestDeps(t)
	t.Setenv("CHATGPT2API_WEB_DIST", t.TempDir())
	deps.Dist = web.NewDist()
	router := api.NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/totally-unknown", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown non-API path = %d, want 404", rec.Code)
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Errorf("content-type = %q, want application/json", contentType)
	}
}
