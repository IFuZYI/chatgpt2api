package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// 详情接口必须同时接受 access_token 与 account_id 两个参数名。
//
// 为什么两个都要：前端既有调用用 access_token；而 token 会轮换，
// 外部系统记下的引用会在轮换后失效 —— account_id 终身不变。
func TestDetailParamResolution(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
		ok    bool
	}{
		{"access_token", "?access_token=at-1", "at-1", true},
		{"account_id", "?account_id=11111111-2222-3333-4444-555555555555", "11111111-2222-3333-4444-555555555555", true},
		{"id 简写", "?id=11111111-2222-3333-4444-555555555555", "11111111-2222-3333-4444-555555555555", true},
		{"两者都给（access_token 优先）", "?access_token=at-1&account_id=xyz", "at-1", true},
		{"都没有", "", "", false},
		{"只有空白", "?access_token=%20%20", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/accounts/detail"+tc.query, nil)

			got, ok := resolveAccountLookup(c)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("解析出 %q，应为 %q", got, tc.want)
			}
		})
	}
}
