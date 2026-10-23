package upstream

import "testing"

// 协议竞速（HTTP/3）只在 SOCKS5 或直连下启用。
//
// 回归点：无条件启用 racing 会让 HTTP/HTTPS 代理在建客户端阶段直接报错
// "protocol racing requires a SOCKS5 proxy for HTTP/3 support"，
// 所有用 HTTP 代理（含仓库默认的 http://privoxy:8118）的用户完全不可用。
func TestSupportsProtocolRacing(t *testing.T) {
	allowed := []string{
		"",                              // 直连
		"socks5://127.0.0.1:1080",       // SOCKS5
		"socks5h://user:pass@host:6740", // SOCKS5H
		"SOCKS5://UPPER:1080",           // 大小写不敏感
	}
	for _, proxy := range allowed {
		if !supportsProtocolRacing(proxy) {
			t.Errorf("%q should allow protocol racing", proxy)
		}
	}

	denied := []string{
		"http://privoxy:8118",
		"https://proxy.example.com:8080",
		"HTTP://UPPER:8080",
	}
	for _, proxy := range denied {
		if supportsProtocolRacing(proxy) {
			t.Errorf("%q must NOT allow protocol racing (HTTP proxy cannot tunnel QUIC)", proxy)
		}
	}
}

// 用真实 tls-client 验证：HTTP 代理 + racing 会报错，而分流后能建起来。
func TestNewClientImplWithHTTPProxyDoesNotFail(t *testing.T) {
	// 只验证「不因代理类型而构造失败」，不发起网络请求。
	for _, proxy := range []string{"http://127.0.0.1:8118", "https://127.0.0.1:8443"} {
		client, _, err := newClientImpl(ClientOptions{
			Impersonate: "chrome110",
			ProxyURL:    proxy,
			TimeoutSec:  0,
		})
		if err != nil {
			t.Errorf("HTTP proxy %q should build a client, got: %v", proxy, err)
			continue
		}
		client.CloseIdleConnections()
	}
}
