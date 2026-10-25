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
func RefreshAccessToken(ctx context.Context, refreshToken string) (map[string]string, error) {
	return RefreshAccessTokenVia(ctx, oauthTokenURL, refreshToken, nil)
}
