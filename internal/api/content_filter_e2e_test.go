package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/service"
)

// 敏感词必须在请求入口拦截，且不消耗任何上游资源。
//
// 回归点：SensitiveWords() 之前只被 /api/settings 回显，请求链路零调用。
// Python: services/content_filter.py:150-157。
func TestSensitiveWordBlocksImageGeneration(t *testing.T) {
	deps := newTestDeps(t)
	deps.ContentFilter = service.NewContentFilter(service.ContentFilterOptions{
		SensitiveWords: func() []string { return []string{"违禁内容"} },
		AIReview:       func() map[string]any { return map[string]any{"enabled": false} },
	})
	router := newTestRouter(t, deps)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"请画违禁内容","n":1}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "敏感词") {
		t.Errorf("body = %q, want it to mention 敏感词", rec.Body.String())
	}
}

// 敏感词同样拦截异步任务提交（不该占用队列）。
func TestSensitiveWordBlocksImageTask(t *testing.T) {
	deps := newTestDeps(t)
	deps.ContentFilter = service.NewContentFilter(service.ContentFilterOptions{
		SensitiveWords: func() []string { return []string{"违禁内容"} },
		AIReview:       func() map[string]any { return map[string]any{"enabled": false} },
	})
	router := newTestRouter(t, deps)

	req := httptest.NewRequest(http.MethodPost, "/api/image-tasks/generations",
		strings.NewReader(`{"prompt":"请画违禁内容","client_task_id":"t1"}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (task should be rejected before enqueue)", rec.Code)
	}
}

// 未命中敏感词时必须放行（不能把正常请求也拦了）。
func TestNormalPromptPassesContentFilter(t *testing.T) {
	deps := newTestDeps(t)
	deps.ContentFilter = service.NewContentFilter(service.ContentFilterOptions{
		SensitiveWords: func() []string { return []string{"违禁内容"} },
		AIReview:       func() map[string]any { return map[string]any{"enabled": false} },
	})
	router := newTestRouter(t, deps)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"一只猫","n":1}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// 没有上游会返回 502，但绝不能是 400 内容拒绝
	if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "敏感词") {
		t.Errorf("normal prompt wrongly rejected: %s", rec.Body.String())
	}
}
