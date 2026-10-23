package service

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// buildProxyTransport 为「探测代理连通性」这类本地诊断构造 transport。
//
// 注意：这不是上游请求用的客户端。上游必须用带指纹的 tls-client
// （见 internal/upstream/client.go 与 ADR-0002）。这里只是拿 csrf 端点
// 判断代理是否可用，Cloudflare 是否放行不影响结论（<500 即算通）。
func buildProxyTransport(proxyURL string, timeout time.Duration) (*http.Transport, error) {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:          32,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ExpectContinueTimeout: 1 * time.Second,
	}
	normalized := normalizeProxyURL(proxyURL)
	if normalized == "" {
		return transport, nil
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return nil, err
	}
	// 标准库不支持 socks5h 前缀，映射到 socks5（Go 的 socks5 本身就是远程解析）。
	if strings.HasPrefix(strings.ToLower(parsed.Scheme), "socks") {
		parsed.Scheme = "socks5"
	}
	transport.Proxy = http.ProxyURL(parsed)
	return transport, nil
}

// urlParse 是 url.Parse 的薄包装，避免在多处重复 import。
func urlParse(raw string) (*url.URL, error) {
	return url.Parse(raw)
}
