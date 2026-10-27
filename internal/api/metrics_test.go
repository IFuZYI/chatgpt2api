package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/api"
)

// /api/metrics 必须鉴权 —— 指标含 goroutine 数、堆大小、运行时长，
// 对攻击者是有用的侦察信息（判断是否被刷、何时重启过）。
func TestMetricsRequiresAdmin(t *testing.T) {
	deps := newTestDeps(t)
	router := api.NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("未鉴权 status = %d, want 401", rec.Code)
	}
}

// 鉴权后返回运行期指标，且 uptime 必须是合理值（不能是从零值算出的天文数字）。
func TestMetricsReturnsRuntimeData(t *testing.T) {
	deps := newTestDeps(t)
	router := api.NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Runtime struct {
			Goroutines int   `json:"goroutines"`
			HeapBytes  int64 `json:"heap_bytes"`
			UptimeSecs int64 `json:"uptime_secs"`
		} `json:"runtime"`
		Accounts map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if payload.Runtime.Goroutines <= 0 {
		t.Errorf("goroutines = %d", payload.Runtime.Goroutines)
	}
	if payload.Runtime.HeapBytes <= 0 {
		t.Errorf("heap_bytes = %d", payload.Runtime.HeapBytes)
	}
	// uptime 应在 [0, 1 天] 内 —— 负值或巨大值都说明 startedAt 没初始化。
	if payload.Runtime.UptimeSecs < 0 || payload.Runtime.UptimeSecs > 86400 {
		t.Errorf("uptime_secs = %d，startedAt 可能未初始化", payload.Runtime.UptimeSecs)
	}
	if payload.Accounts == nil {
		t.Error("accounts 段缺失")
	}
}
