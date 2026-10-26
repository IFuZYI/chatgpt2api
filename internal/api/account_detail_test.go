package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// doAccountDetail 发一次详情请求。
func doAccountDetail(t *testing.T, engine http.Handler, authHeader, accessToken string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	target := "/api/accounts/detail"
	if accessToken != "" {
		target += "?access_token=" + accessToken
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	body := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec, body
}

// 详情接口必须存在且对 admin 返回完整账号（含凭据原文）。
//
// 这是新功能的入口契约：路由缺失时前端「查看」按钮点了只会 404。
func TestAccountDetailRouteReturnsFullAccount(t *testing.T) {
	deps := newTestDeps(t)
	engine := newTestRouter(t, deps)

	seed := map[string]any{
		"access_token":  "tok-detail-1",
		"type":          "Plus",
		"status":        "正常",
		"quota":         42,
		"email":         "detail@example.com",
		"password":      "secret-password",
		"totp_secret":   "JBSWY3DPEHPK3PXP",
		"refresh_token": "rt-value",
		"id_token":      "id-value",
		"session_token": "session-value",
		"proxy":         "socks5h://10.0.0.9:1080",
	}
	deps.Accts.AddAccountItems([]map[string]any{seed}, nil)

	rec, body := doAccountDetail(t, engine, "Bearer test", "tok-detail-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	item, ok := body["item"].(map[string]any)
	if !ok {
		t.Fatalf("response has no item object: %s", rec.Body.String())
	}

	// 详情与列表的关键差异：凭据原文必须在。
	for field, want := range map[string]string{
		"password":      "secret-password",
		"totp_secret":   "JBSWY3DPEHPK3PXP",
		"refresh_token": "rt-value",
		"id_token":      "id-value",
		"session_token": "session-value",
	} {
		if got, _ := item[field].(string); got != want {
			t.Errorf("item.%s = %q, want %q", field, got, want)
		}
	}

	// 派生字段也在。
	for _, field := range []string{
		"access_token_expires_in", "access_token_expires_at",
		"proxy_source", "proxy_effective", "has_password",
	} {
		if _, ok := item[field]; !ok {
			t.Errorf("item.%s is missing", field)
		}
	}
}

// 未鉴权必须 401，且不能泄漏任何账号字段。
func TestAccountDetailRequiresAuth(t *testing.T) {
	deps := newTestDeps(t)
	engine := newTestRouter(t, deps)
	deps.Accts.AddAccountItems([]map[string]any{
		{"access_token": "tok-auth", "type": "free", "status": "正常", "password": "leak-me"},
	}, nil)

	rec, body := doAccountDetail(t, engine, "", "tok-auth")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "leak-me") {
		t.Fatalf("401 response leaked credentials: %s", rec.Body.String())
	}
	if _, ok := body["item"]; ok {
		t.Fatalf("401 response contained an item: %s", rec.Body.String())
	}
}

// 普通用户 key 不能读详情（这是 admin-only 接口）。
//
// 理由：详情含 password / totp_secret 原文，等同账号完全接管。
// 列表接口对 user 也关闭，详情更不能开。
func TestAccountDetailForbiddenForUserRole(t *testing.T) {
	deps := newTestDeps(t)
	engine := newTestRouter(t, deps)
	deps.Accts.AddAccountItems([]map[string]any{
		{"access_token": "tok-role", "type": "free", "status": "正常", "password": "admin-only"},
	}, nil)

	_, rawKey, err := deps.Auth.CreateKey("user", "test-user")
	if err != nil {
		t.Fatalf("create user key: %v", err)
	}

	rec, _ := doAccountDetail(t, engine, "Bearer "+rawKey, "tok-role")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "admin-only") {
		t.Fatalf("403 response leaked credentials: %s", rec.Body.String())
	}
}

// 缺 access_token 参数返回 400（不是 404 —— 那是「账号不存在」的语义）。
func TestAccountDetailMissingParamIs400(t *testing.T) {
	deps := newTestDeps(t)
	engine := newTestRouter(t, deps)

	rec, _ := doAccountDetail(t, engine, "Bearer test", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// 账号不存在返回 404。
func TestAccountDetailUnknownTokenIs404(t *testing.T) {
	deps := newTestDeps(t)
	engine := newTestRouter(t, deps)

	rec, _ := doAccountDetail(t, engine, "Bearer test", "nope-not-in-pool")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
