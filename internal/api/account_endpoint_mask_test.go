package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/api"
)

// 端点级脱敏守卫：所有返回账号列表的端点都不能泄漏凭据。
//
// 这是**集成**测试 —— 之前的 account_mask_test.go 只测了脱敏函数本身，
// 测不出「handler 忘了调用它」。DELETE /api/accounts 的泄漏正是这样漏掉的。
func TestAccountListEndpointsDoNotLeakCredentials(t *testing.T) {
	deps := newTestDeps(t)
	router := api.NewRouter(deps)

	// 先导入一个带全部凭据的账号
	seed := `{"accounts":[{"access_token":"at-leak-check","email":"leak@x.test",
		"password":"SECRET-PW","totp_secret":"JBSWY3DPEHPK3PXP",
		"refresh_token":"SECRET-RT","id_token":"SECRET-ID","session_token":"SECRET-SESS"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewBufferString(seed))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("导入失败: %d %s", w.Code, w.Body.String())
	}

	secretFields := []string{"password", "totp_secret", "refresh_token", "id_token", "session_token"}

	check := func(name string, code int, body []byte) {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("%s 返回 %d: %s", name, code, string(body))
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("%s 响应不是 JSON: %v", name, err)
		}
		items, _ := payload["items"].([]any)
		if len(items) == 0 {
			if item, ok := payload["item"].(map[string]any); ok {
				items = []any{item}
			}
		}
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			for _, field := range secretFields {
				if value, present := item[field]; present && value != nil && value != "" {
					t.Errorf("%s 泄漏 %s = %v", name, field, value)
				}
			}
		}
	}

	// GET /api/accounts
	req = httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	req.Header.Set("Authorization", "Bearer test")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	check("GET /api/accounts", w.Code, w.Body.Bytes())

	// POST /api/accounts（上传响应）
	req = httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewBufferString(seed))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	check("POST /api/accounts", w.Code, w.Body.Bytes())

	// POST /api/accounts/update
	req = httptest.NewRequest(http.MethodPost, "/api/accounts/update",
		bytes.NewBufferString(`{"access_token":"at-leak-check","quota":1}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	check("POST /api/accounts/update", w.Code, w.Body.Bytes())

	// POST /api/accounts/proxy
	req = httptest.NewRequest(http.MethodPost, "/api/accounts/proxy",
		bytes.NewBufferString(`{"access_tokens":["at-leak-check"],"proxy":""}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	check("POST /api/accounts/proxy", w.Code, w.Body.Bytes())

	// DELETE /api/accounts —— 这一条曾经漏掉
	req = httptest.NewRequest(http.MethodDelete, "/api/accounts",
		bytes.NewBufferString(`{"tokens":["nonexistent-xyz"]}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	check("DELETE /api/accounts", w.Code, w.Body.Bytes())
}

// 刷新进度接口不能泄漏凭据。
//
// 回归点：RefreshAccounts 的 result 里带 "items": s.ListAccounts()，
// 而 FinishRefreshProgress 把它整个存进 Progress.Result，
// GET /api/accounts/refresh/progress/:id 原样返回 ——
// 刷新**一个**账号会泄漏**全池**的 password/totp_secret/refresh_token，
// 包括这次根本没被刷新的账号。
func TestRefreshProgressDoesNotLeakCredentials(t *testing.T) {
	deps := newTestDeps(t)
	router := api.NewRouter(deps)

	seed := `{"accounts":[{"access_token":"at-prog","email":"p@x.test",
		"password":"SECRET-PW","totp_secret":"JBSWY3DPEHPK3PXP","refresh_token":"SECRET-RT"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewBufferString(seed))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("导入失败: %d", w.Code)
	}

	// 触发刷新，拿 progress_id
	req = httptest.NewRequest(http.MethodPost, "/api/accounts/refresh",
		bytes.NewBufferString(`{"tokens":["at-prog"]}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("刷新请求失败: %d %s", w.Code, w.Body.String())
	}
	var started map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &started)
	progressID, _ := started["progress_id"].(string)
	if progressID == "" {
		t.Fatalf("没有 progress_id: %s", w.Body.String())
	}

	// 等刷新结束（可能很快失败，无所谓 —— 我们要看的是响应里有没有凭据）
	var body []byte
	for i := 0; i < 40; i++ {
		req = httptest.NewRequest(http.MethodGet, "/api/accounts/refresh/progress/"+progressID, nil)
		req.Header.Set("Authorization", "Bearer test")
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		body = w.Body.Bytes()
		var payload map[string]any
		if json.Unmarshal(body, &payload) == nil {
			if done, _ := payload["done"].(bool); done {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	// 断言：整个响应体里不能出现凭据原文
	for _, secret := range []string{"SECRET-PW", "JBSWY3DPEHPK3PXP", "SECRET-RT"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("刷新进度响应泄漏 %q —— 刷一个账号泄漏全池", secret)
		}
	}
}

// 刷新「所有 token 都在飞」的提前返回分支不能泄漏凭据。
//
// 回归点：RefreshAccounts 在 len(unique)==0 时构造的 result 里带
// "items": ListAccounts()（全池原始记录含 password/totp_secret/
// refresh_token），而 FinishRefreshProgress 把它存进 Progress.Result，
// GET /api/accounts/refresh/progress/:id 原样返回。
// 同函数末尾的非空分支已修过，这个 early-return 漏了。
//
// 用可阻塞的 verifier 占住第一次刷新，第二次同 token 即 unique 为空。
func TestRefreshWithAllTokensInFlightDoesNotLeak(t *testing.T) {
	deps := newTestDeps(t)
	router := api.NewRouter(deps)

	seed := `{"accounts":[{"access_token":"at-inflight","email":"i@x.test",
		"password":"SECRET-PW","totp_secret":"JBSWY3DPEHPK3PXP","refresh_token":"SECRET-RT"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewBufferString(seed))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("导入失败: %d", w.Code)
	}

	// 第一次刷新：后台跑，会阻塞在上游（测试环境无网，天然慢）。
	req = httptest.NewRequest(http.MethodPost, "/api/accounts/refresh",
		bytes.NewBufferString(`{"access_tokens":["at-inflight"]}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var first map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &first)

	// 立刻再刷同一个 token —— 极可能仍处于在飞状态
	req = httptest.NewRequest(http.MethodPost, "/api/accounts/refresh",
		bytes.NewBufferString(`{"access_tokens":["at-inflight"]}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("第二次刷新失败: %d", w.Code)
	}
	var second map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &second)

	// 两个进度都要检查
	for _, payload := range []map[string]any{first, second} {
		progressID, _ := payload["progress_id"].(string)
		if progressID == "" {
			continue
		}
		for i := 0; i < 30; i++ {
			req = httptest.NewRequest(http.MethodGet, "/api/accounts/refresh/progress/"+progressID, nil)
			req.Header.Set("Authorization", "Bearer test")
			w = httptest.NewRecorder()
			router.ServeHTTP(w, req)
			for _, secret := range []string{"SECRET-PW", "JBSWY3DPEHPK3PXP", "SECRET-RT"} {
				if bytes.Contains(w.Body.Bytes(), []byte(secret)) {
					t.Errorf("刷新进度泄漏 %q —— 提前返回分支未脱敏", secret)
				}
			}
			var body map[string]any
			if json.Unmarshal(w.Body.Bytes(), &body) == nil {
				if done, _ := body["done"].(bool); done {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
}
