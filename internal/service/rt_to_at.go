package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RefreshTokenRejectedError 表示上游**明确拒绝**了这个 refresh_token。
//
// 与网络故障的区别很重要：拒绝意味着 RT 已经死了，重试没有意义，必须重新登录；
// 网络故障只需要等下一次 tick 重试。把两者混在一起会导致
// 「RT 死了却一直重试」或者「网络抖一下就触发密码登录」。
type RefreshTokenRejectedError struct {
	StatusCode int
	Detail     string
}

func (e *RefreshTokenRejectedError) Error() string {
	return fmt.Sprintf("oauth_refresh_http_%d: %s", e.StatusCode, e.Detail)
}

// IsRefreshTokenRejected 报告错误是否属于「RT 被上游拒绝」。
//
// 400/401/403 视为拒绝（RT 无效、被吊销、账号受限）。
// 429 是限流、5xx 是上游故障、网络错误是本地问题 —— 都不算拒绝，应当重试。
func IsRefreshTokenRejected(err error) bool {
	var rejected *RefreshTokenRejectedError
	if errors.As(err, &rejected) {
		return true
	}
	return false
}

// RefreshAccessTokenVia 用 refresh_token 换一套新 token。
//
// 与 exchangeOAuthCodeVia 的区别（两者都是 POST /oauth/token，但形状不同）：
//   - 换 code：JSON body，grant_type=authorization_code，带 code_verifier
//   - 换 token：form-urlencoded body，grant_type=refresh_token，带 client_id
//
// 混用会拿到 400。target 可注入以便测试；生产用 RefreshAccessToken。
func RefreshAccessTokenVia(
	ctx context.Context,
	target, refreshToken string, client *http.Client,
) (map[string]string, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("refresh_token is empty")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", oauthClientIDConst)

	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", oauthUserAgentConst)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh_token 换 access_token 网络异常: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, fmt.Errorf("读取 oauth/token 响应失败: %w", readErr)
	}

	var data map[string]any
	_ = json.Unmarshal(body, &data)

	accessToken := ""
	if data != nil {
		accessToken = strings.TrimSpace(stringValue(data["access_token"]))
	}
	if resp.StatusCode != http.StatusOK || accessToken == "" {
		detail := ""
		if data != nil {
			for _, key := range []string{"error_description", "error", "message"} {
				if v := strings.TrimSpace(stringValue(data[key])); v != "" {
					detail = v
					break
				}
			}
		}
		if detail == "" && len(body) > 0 {
			detail = strings.TrimSpace(string(body))
			if len(detail) > 300 {
				detail = detail[:300]
			}
		}
		if detail == "" {
			detail = "上游未返回 detail"
		}
		if resp.StatusCode == http.StatusBadRequest ||
			resp.StatusCode == http.StatusUnauthorized ||
			resp.StatusCode == http.StatusForbidden {
			return nil, &RefreshTokenRejectedError{StatusCode: resp.StatusCode, Detail: detail}
		}
		return nil, fmt.Errorf("oauth_refresh_http_%d: %s", resp.StatusCode, detail)
	}

	// 上游可能轮换 refresh_token（返回新的）；没返回就沿用旧的。
	newRefresh := refreshToken
	if v := strings.TrimSpace(stringValue(data["refresh_token"])); v != "" {
		newRefresh = v
	}
	idToken := ""
	if data != nil {
		idToken = strings.TrimSpace(stringValue(data["id_token"]))
	}
	return map[string]string{
		"access_token":  accessToken,
		"refresh_token": newRefresh,
		"id_token":      idToken,
	}, nil
}

// RefreshAccessToken 用生产端点换 token。
// RefreshAccessToken 用生产端点换 token。
//
// proxyURL 是该账号的出口代理（空串 = 直连）。必须建一个走该代理的
// http.Client，不能传 nil —— nil 会落到裸 &http.Client{}，只吃环境变量
// HTTP(S)_PROXY，等于让 RT 续期从一个不受控的出口发出。
func RefreshAccessToken(ctx context.Context, proxyURL, refreshToken string) (map[string]string, error) {
	client, err := newProxyHTTPClient(proxyURL, 60*time.Second)
	if err != nil {
		return nil, err
	}
	// 回收空闲连接。
	//
	// 每次 keepalive 都新建一个 transport；不回收的话，池规模 300+、
	// watcher 每轮 tick 一次，会向代理端持续累积空闲连接（靠
	// IdleConnTimeout=30s 兜底，不是无限泄漏，但每轮都会短暂堆积
	// 最多 MaxIdleConns×N 条）。对照 upstream.Client.Close 是有做的。
	defer client.CloseIdleConnections()
	return RefreshAccessTokenVia(ctx, oauthTokenURL, refreshToken, client)
}

// withDirectHTTPClient 构造一个直连 http.Client，执行 fn 后回收空闲连接。
//
// 抽出来是为了让「用完回收」这件事可测试：连接回收是无观测副作用的，
// 直接断言 client 内部状态等于测实现；用一个带 ConnState 计数的
// httptest 服务端观察"连接是否真的被关掉"才是行为断言。
func withDirectHTTPClient(timeout time.Duration, fn func(*http.Client) error) error {
	client := &http.Client{Timeout: timeout}
	defer client.CloseIdleConnections()
	return fn(client)
}

// newProxyHTTPClient 按代理 URL 构造一个 http.Client（空串则直连）。
//
// 支持 http/https/socks5/socks5h；socks 系走 golang.org/x/net/proxy 的
// Dialer，与 tls-client 侧对 socks5h 的处理保持同一语义（远程 DNS）。
func newProxyHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	normalized := normalizeProxyURL(proxyURL)
	if normalized == "" {
		return &http.Client{Timeout: timeout}, nil
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		// 必须脱敏：url.Parse 的错误文本里含整个 URL，包括 user:password。
		// 这条错误会经 RecordTokenRefreshError / recordReloginFailure 落盘到
		// accounts.json 的 last_*_error 字段，并由 GET /api/accounts 返回、
		// 在详情弹窗里可复制 —— 而那 5 个脱敏字段不含 last_*_error。
		// 也就是说：不脱敏 = 把代理密码写进面板。
		return nil, fmt.Errorf("解析代理地址失败: %s", redactURLCredentials(err.Error()))
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyURL(parsed),
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		// 必须显式开启：设置 DialContext 会让 Go 关闭自动 HTTP/2
		// （见 transport.go："Be conservative and don't automatically
		// enable http2 if they've specified ... custom dialers"）。
		// socks5h 分支设了 DialContext，不开这一项就会退到 HTTP/1.1，
		// 于是同一产品内 http 代理走 H2、socks5h 走 H1.1 —— 上游指纹不一致。
		ForceAttemptHTTP2: true,
	}
	// socks5/socks5h 不能走 http.Transport.Proxy（它只认 http/https 代理）。
	switch strings.ToLower(parsed.Scheme) {
	case "socks5", "socks5h":
		dialer, dialErr := newSOCKS5Dialer(parsed, timeout)
		if dialErr != nil {
			return nil, dialErr
		}
		transport.Proxy = nil
		transport.DialContext = dialer
	}
	return &http.Client{Timeout: timeout, Transport: transport}, nil
}
