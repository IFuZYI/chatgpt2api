package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// n 越界必须报校验错误，不能静默夹取后打到上游。
//
// Python 的 pydantic 是 `n: int = Field(default=1, ge=1, le=4)`，
// 越界返回 422（Pydantic 条目形状）。Go 侧曾用 clampImageCount 静默
// 把 0 夹成 1、5 夹成 4，然后真的去生图 —— 用户以为参数生效了。
func TestImageGenerationsRejectsOutOfRangeN(t *testing.T) {
	deps := newTestDeps(t)
	router := newTestRouter(t, deps)

	cases := []struct {
		body string
		want int
	}{
		{`{"prompt":"x","n":0}`, http.StatusUnprocessableEntity},
		{`{"prompt":"x","n":5}`, http.StatusUnprocessableEntity},
		{`{"prompt":"x","n":-1}`, http.StatusUnprocessableEntity},
		{`{"prompt":"x","n":"abc"}`, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer test")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != tc.want {
			t.Errorf("%s = %d, want %d (body: %s)", tc.body, rec.Code, tc.want, rec.Body.String()[:min(120, len(rec.Body.String()))])
		}
	}
}

// edits 接口的 n 越界是 400（Python 的 _parse_count 用 HTTPException(400)）。
func TestImageEditsRejectsOutOfRangeN(t *testing.T) {
	deps := newTestDeps(t)
	router := newTestRouter(t, deps)

	for _, body := range []string{`{"prompt":"x","n":0}`, `{"prompt":"x","n":5}`} {
		req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", body, rec.Code)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
