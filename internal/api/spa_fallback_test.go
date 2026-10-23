package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 未知的 /api/* 与 /v1/* 必须返回 404 JSON，不能被 SPA fallback 吞成 200 HTML。
//
// 回归点：NoRoute 对所有 GET 都走 spa()，于是拼错路径的客户端拿到
// 200 + 一段 HTML 而不是 JSON 错误。OpenAI SDK 会把 HTML 当成功响应解析失败。
// FastAPI 对未知 API 路径返回 404 {"detail":"Not Found"}。
func TestUnknownAPIPathsReturnJSON404(t *testing.T) {
	deps := newTestDeps(t)
	router := newTestRouter(t, deps)

	cases := []string{
		"/api/nonexistent-endpoint",
		"/api/third-party-apps",
		"/v1/nonexistent-endpoint",
		"/v1/messages/extra",
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
func TestSPARoutesStillServeHTML(t *testing.T) {
	deps := newTestDeps(t)
	router := newTestRouter(t, deps)

	for _, path := range []string{"/", "/accounts", "/settings", "/image"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		contentType := rec.Header().Get("Content-Type")
		if !strings.Contains(contentType, "text/html") {
			t.Errorf("GET %s content-type = %q, want text/html", path, contentType)
		}
	}
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
