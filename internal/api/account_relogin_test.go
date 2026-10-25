package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/service"
)

// passwordLoginStub 是 api 层测试用的假认证上游：所有 POST 都回 200，
// 按路径回放脚本。它存在的意义是让 /api/accounts/re-login 端到端跑通，
// 同时断言「确实走到了上游、且拿到了新 token」。
type passwordLoginStub struct {
	requests []passwordLoginStubRequest
}

type passwordLoginStubRequest struct {
	target string
	body   []byte
}

func (s *passwordLoginStub) AuthGet(ctx context.Context, target string, headers map[string]string, timeout time.Duration) ([]byte, int, string, error) {
	s.requests = append(s.requests, passwordLoginStubRequest{target: target})
	return []byte("<html>password page</html>"), 200, "https://auth.openai.com/log-in/password", nil
}

func (s *passwordLoginStub) AuthPost(ctx context.Context, target string, headers map[string]string, payload []byte, timeout time.Duration) ([]byte, int, error) {
	s.requests = append(s.requests, passwordLoginStubRequest{target: target, body: payload})
	switch {
	case strings.Contains(target, "/api/accounts/password/verify"):
		return []byte(`{"continue_url":"https://platform.openai.com/auth/callback?code=e2e-code"}`), 200, nil
	case strings.Contains(target, "/api/accounts/oauth/token"):
		return []byte(`{"access_token":"e2e-access","refresh_token":"e2e-refresh","id_token":"e2e-id"}`), 200, nil
	}
	return []byte(`{}`), 200, nil
}

func (s *passwordLoginStub) BuildSentinelToken(ctx context.Context, deviceID, flow string) (string, string, error) {
	return `{"p":"p","t":"","c":"c","id":"` + deviceID + `","flow":"` + flow + `"}`, "0c", nil
}

func (s *passwordLoginStub) SetSessionCookie(domain, name, value string) {}

func (s *passwordLoginStub) Close() {}

// 请求一律打到假上游：测试里绝不能碰真实 auth.openai.com。
func TestReLoginEndpointRunsPasswordFlow(t *testing.T) {
	deps := newTestDeps(t)
	stub := &passwordLoginStub{}
	deps.PasswordLoginClient = func() (service.PasswordLoginTransport, error) { return stub, nil }

	seed := deps.Accts.AddAccountItems([]map[string]any{{
		"access_token": "stale-token",
		"email":        "e2e@example.com",
		"password":     "pw",
		"totp_secret":  "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ",
		"status":       service.StatusAbnormal,
	}}, nil)
	if seed["added"] != 1 {
		t.Fatalf("seeded = %v", seed)
	}

	router := newTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/re-login",
		strings.NewReader(`{"access_tokens":["stale-token"]}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var started map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	progressID, _ := started["progress_id"].(string)
	if progressID == "" {
		t.Fatal("progress_id missing")
	}

	// 轮询进度直到 done（后台 goroutine 串行处理，最多几十毫秒）。
	progress := pollReloginProgress(t, router, progressID)
	if progress["done"] != true {
		t.Fatalf("progress = %v, want done", progress)
	}
	entries, _ := progress["results"].([]any)
	if len(entries) != 1 {
		t.Fatalf("results = %v", progress["results"])
	}
	entry, _ := entries[0].(map[string]any)
	if entry["status"] != "成功" {
		t.Fatalf("status = %v (error=%v), want 成功", entry["status"], entry["error"])
	}
	if !strings.Contains(entry["token"].(string), "token:") {
		t.Errorf("token should be anonymized: %v", entry["token"])
	}

	// 账号拿到新 token + refresh_token，source_type 变 password。
	account := deps.Accts.GetAccount("e2e-access")
	if account == nil {
		t.Fatal("relogin access_token not written back")
	}
	if account["refresh_token"] != "e2e-refresh" {
		t.Errorf("refresh_token = %v", account["refresh_token"])
	}
	if account["status"] != service.StatusNormal {
		t.Errorf("status = %v, want 正常", account["status"])
	}
	if account["source_type"] != service.SourcePassword {
		t.Errorf("source_type = %v, want password", account["source_type"])
	}

	// 必须真的走了 authorize → password/verify → oauth/token。
	var sawAuthorize, sawVerify, sawExchange bool
	for _, request := range stub.requests {
		switch {
		case strings.Contains(request.target, "/api/accounts/authorize"):
			sawAuthorize = true
		case strings.Contains(request.target, "/api/accounts/password/verify"):
			sawVerify = true
		case strings.Contains(request.target, "/api/accounts/oauth/token"):
			sawExchange = true
		}
	}
	if !sawAuthorize || !sawVerify || !sawExchange {
		t.Errorf("upstream calls incomplete: %+v", stub.requests)
	}
}

// 缺 password 的账号：进度里必须出现 no_password_in_account，且不碰上游。
func TestReLoginEndpointReportsMissingPassword(t *testing.T) {
	deps := newTestDeps(t)
	calls := 0
	deps.PasswordLoginClient = func() (service.PasswordLoginTransport, error) {
		calls++
		return &passwordLoginStub{}, nil
	}
	deps.Accts.AddAccountItems([]map[string]any{{
		"access_token": "no-pw-token",
		"email":        "nopw@example.com",
		"status":       service.StatusAbnormal,
	}}, nil)

	router := newTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/re-login",
		strings.NewReader(`{"access_tokens":["no-pw-token"]}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var started map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	progress := pollReloginProgress(t, router, started["progress_id"].(string))

	entries, _ := progress["results"].([]any)
	if len(entries) != 1 {
		t.Fatalf("results = %v", progress["results"])
	}
	entry := entries[0].(map[string]any)
	if entry["status"] != "跳过" {
		t.Errorf("status = %v, want 跳过", entry["status"])
	}
	if !strings.Contains(entry["error"].(string), service.PasswordLoginNoPassword) {
		t.Errorf("error = %v, want %s", entry["error"], service.PasswordLoginNoPassword)
	}
	if calls != 0 {
		t.Errorf("transport factory called %d times for an account without a password", calls)
	}
}

// 未注入上游时要如实报 password_login_not_configured（不能假装成功）。
func TestReLoginEndpointWithoutUpstream(t *testing.T) {
	deps := newTestDeps(t)
	deps.Accts.AddAccountItems([]map[string]any{{
		"access_token": "tok-x",
		"email":        "x@example.com",
		"password":     "pw",
	}}, nil)
	router := newTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/re-login",
		strings.NewReader(`{"access_tokens":["tok-x"]}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var started map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	progress := pollReloginProgress(t, router, started["progress_id"].(string))
	entries := progress["results"].([]any)
	if len(entries) != 1 {
		t.Fatalf("results = %v", progress["results"])
	}
	if !strings.Contains(entries[0].(map[string]any)["error"].(string), "password_login_not_configured") {
		t.Errorf("error = %v", entries[0])
	}
}

// re-login 仍然要求 admin 鉴权。
func TestReLoginEndpointRequiresAdmin(t *testing.T) {
	deps := newTestDeps(t)
	router := newTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/re-login",
		strings.NewReader(`{"access_tokens":["x"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// pollReloginProgress 轮询进度接口直到 done。
//
// 上限给到 30 秒：整包 `go test ./...` 时并行包会把 CPU 占满，
// 后台 goroutine 被调度延迟到几秒是常态（实测踩到 2 秒上限偶发超时）。
// 这个上限只用来兜住真正的挂死，不是性能断言。
func pollReloginProgress(t *testing.T, router http.Handler, progressID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		req := httptest.NewRequest(http.MethodGet, "/api/accounts/re-login/progress/"+progressID, nil)
		req.Header.Set("Authorization", "Bearer test")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("progress status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var progress map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &progress); err != nil {
			t.Fatalf("decode progress: %v", err)
		}
		if progress["done"] == true {
			return progress
		}
		if time.Now().After(deadline) {
			t.Fatalf("relogin progress never finished: %v", progress)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
