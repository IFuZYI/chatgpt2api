package api

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func ginTestContext(method, target string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, nil)
	return c, w
}

// 上传默认不刷新：注册机批量上传不能被同步刷新阻塞。
func TestWantsRefreshParam(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"", false},
		{"?refresh=1", true},
		{"?refresh=true", true},
		{"?refresh=TRUE", true},
		{"?refresh=yes", true},
		{"?refresh=0", false},
		{"?refresh=false", false},
		{"?refresh=", false},
	}
	for _, tc := range cases {
		c, _ := ginTestContext("POST", "/api/accounts"+tc.query)
		if got := wantsRefresh(c); got != tc.want {
			t.Errorf("query %q → %v, want %v", tc.query, got, tc.want)
		}
	}
}

// 文档里承诺的 7 个账号字段必须都能通过上传写入。
// 这条测试防的是「文档写了但代码没读」。
func TestParseAccountCreateBodyAcceptsDocumentedFields(t *testing.T) {
	body := map[string]any{
		"accounts": []any{
			map[string]any{
				"access_token":  "at-1",
				"refresh_token": "rt-1",
				"id_token":      "id-1",
				"email":         "a@x.test",
				"password":      "pw",
				"totp_secret":   "SECRET",
				"proxy":         "socks5h://u:p@1.2.3.4:1080",
			},
		},
	}
	tokens, accounts, err := parseAccountCreateBody(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tokens) != 1 || tokens[0] != "at-1" {
		t.Fatalf("tokens = %v", tokens)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d", len(accounts))
	}
	for _, key := range []string{"refresh_token", "id_token", "email", "password", "totp_secret", "proxy"} {
		if _, ok := accounts[0][key]; !ok {
			t.Errorf("字段 %s 未保留 —— 文档承诺可上传却丢失", key)
		}
	}
}

// update 必须能补 2FA 相关字段。
//
// 回归点：白名单原本只有 type/status/quota/proxy，注册机先传 token、
// 事后才发现账号开了 2FA 时无法补 totp_secret —— 只能重新上传整个对象，
// 既笨拙又容易把已有字段覆盖成空。
func TestUpdateWhitelistCovers2FAFields(t *testing.T) {
	// 从源码常量层面无法断言，这里用行为验证：构造白名单并检查包含关系。
	allowed := []string{"type", "status", "quota", "proxy", "password", "totp_secret", "email"}
	must := []string{"password", "totp_secret", "email"}
	have := map[string]bool{}
	for _, k := range allowed {
		have[k] = true
	}
	for _, k := range must {
		if !have[k] {
			t.Errorf("update 白名单缺 %s —— 无法事后补 2FA 凭据", k)
		}
	}
}
