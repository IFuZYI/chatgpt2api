package service

import "testing"

// 账号级代理必须能按 access_token 取到，且为空时回退全局代理。
//
// 背景：账号记录里的 proxy 字段此前只被显示层读取（GetProfile →
// RuntimeStatus → /health），从未参与真实出站请求 —— 7 处上游客户端
// 全部只用 config.json 的全局 proxy。注册机给每个账号配不同出口 IP
// 时，那些代理完全不生效。
func TestProxyForAccount(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-with-proxy", "type": "free", "status": StatusNormal,
			"proxy": "socks5h://u:p@10.0.0.1:1080"},
		{"access_token": "at-no-proxy", "type": "free", "status": StatusNormal},
	})
	if _, err := cfg.Update(map[string]any{"proxy": "http://global:8080"}); err != nil {
		t.Fatalf("update: %v", err)
	}

	cases := []struct {
		token string
		want  string
		why   string
	}{
		{"at-with-proxy", "socks5h://u:p@10.0.0.1:1080", "账号自己的代理优先"},
		{"at-no-proxy", "http://global:8080", "账号没配则回退全局"},
		{"unknown-token", "http://global:8080", "未知账号回退全局"},
		{"", "http://global:8080", "空 token 回退全局"},
	}
	for _, tc := range cases {
		if got := svc.ProxyFor(tc.token); got != tc.want {
			t.Errorf("ProxyFor(%q) = %q, want %q（%s）", tc.token, got, tc.want, tc.why)
		}
	}
}

// 账号代理为空白字符时也要回退，不能把 "   " 当代理传给 HTTP 客户端。
func TestProxyForIgnoresBlankAccountProxy(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-blank", "type": "free", "status": StatusNormal, "proxy": "   "},
	})
	if _, err := cfg.Update(map[string]any{"proxy": "http://global:8080"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := svc.ProxyFor("at-blank"); got != "http://global:8080" {
		t.Errorf("ProxyFor = %q，空白代理应回退全局", got)
	}
}
