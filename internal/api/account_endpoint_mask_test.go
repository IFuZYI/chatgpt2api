package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
