package service

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// newSOCKS5Dialer 为 socks5/socks5h 代理构造一个 DialContext 函数。
//
// 为什么不能直接用 http.Transport.Proxy：它只处理 http/https 代理，
// 遇到 socks5:// 会返回 "unsupported protocol scheme"。而本仓库的账号
// 代理绝大多数是 socks5h://（远程 DNS），必须走 x/net/proxy 的拨号器。
//
// socks5h 与 socks5 的差别是 DNS 在哪一侧解析：h = hostname 交给代理
// 解析（远程 DNS）。x/net/proxy 的 socks5 Dialer 本身就把域名原样交给
// 代理，因此两种 scheme 共用同一实现即可。
func newSOCKS5Dialer(parsed *url.URL, timeout time.Duration) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	if parsed.Host == "" {
		return nil, fmt.Errorf("socks 代理缺少 host")
	}
	var auth *proxy.Auth
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		auth = &proxy.Auth{User: parsed.User.Username(), Password: password}
	}
	base, err := proxy.SOCKS5("tcp", parsed.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("构造 socks5 拨号器失败: %w", err)
	}
	ctxDialer, ok := base.(proxy.ContextDialer)
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		// 尊重调用方的 deadline；没有 deadline 时套一层兜底超时，
		// 避免一个挂死的代理把 watcher 的 tick 永久卡住
		// （审计发现：timeout 形参原本完全没被使用）。
		if timeout > 0 {
			if _, hasDeadline := ctx.Deadline(); !hasDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
		}
		if ok {
			return ctxDialer.DialContext(ctx, network, addr)
		}
		// 兜底：x/net/proxy 的 SOCKS5 在较新版本里实现了 ContextDialer；
		// 万一没有，用带超时的 net.Dialer 包一层，避免永久挂住。
		type result struct {
			conn net.Conn
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			conn, dialErr := base.Dial(network, addr)
			ch <- result{conn, dialErr}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-ch:
			return r.conn, r.err
		}
	}, nil
}
