package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// makeTestJWT 造一个只有 payload 有意义的 JWT（签名不校验）。
//
// decodeJWTPayload 只读第二段，所以 header/sig 用固定值即可。
func makeTestJWT(t *testing.T, exp int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf(`{"exp":%d,"iat":%d}`, exp, exp-3600)))
	return header + "." + payload + ".sig"
}

// Detail 必须返回凭据原文，否则「查看」弹窗里的密码/2FA 是空的。
//
// 这是与列表接口的刻意差异：/api/accounts 走 maskAccountCredentials 抹掉
// password/totp_secret，详情接口是显式单条查询，必须给全。
func TestAccountDetailIncludesCredentialPlaintext(t *testing.T) {
	exp := time.Now().Add(1 * time.Hour).Unix()
	accessToken := makeTestJWT(t, exp)

	svc, _ := newTestAccountService(t, []map[string]any{
		{
			"access_token":  accessToken,
			"type":          "Plus",
			"status":        StatusNormal,
			"quota":         42,
			"email":         "someone@example.com",
			"password":      "p@ssw0rd-plain",
			"totp_secret":   "JBSWY3DPEHPK3PXP",
			"refresh_token": "rt-plain-value",
			"id_token":      "id-plain-value",
			"session_token": "sess-plain-value",
			"proxy":         "socks5h://user:pass@10.0.0.9:1080",
		},
	})

	detail := svc.Detail(accessToken)
	if detail == nil {
		t.Fatalf("Detail returned nil for an existing account")
	}

	for field, want := range map[string]string{
		"password":      "p@ssw0rd-plain",
		"totp_secret":   "JBSWY3DPEHPK3PXP",
		"refresh_token": "rt-plain-value",
		"id_token":      "id-plain-value",
		"session_token": "sess-plain-value",
	} {
		if got := stringValue(detail[field]); got != want {
			t.Errorf("%s = %q, want %q (detail must expose plaintext credentials)", field, got, want)
		}
	}
}

// AT 剩余有效期必须由 JWT exp 算出来，前端不该自己解析 token。
func TestAccountDetailComputesAccessTokenExpiry(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Unix()
	accessToken := makeTestJWT(t, exp)

	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": accessToken, "type": "free", "status": StatusNormal},
	})

	detail := svc.Detail(accessToken)
	if detail == nil {
		t.Fatalf("Detail returned nil")
	}

	remaining := intValue(detail["access_token_expires_in"])
	// 2 小时 = 7200 秒，允许 60 秒误差（测试执行时间）。
	if remaining < 7100 || remaining > 7260 {
		t.Errorf("access_token_expires_in = %d, want ~7200", remaining)
	}

	expiresAt := stringValue(detail["access_token_expires_at"])
	if expiresAt == "" {
		t.Fatalf("access_token_expires_at is empty; frontend cannot render an absolute time")
	}
	parsed, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		t.Fatalf("access_token_expires_at %q is not RFC3339: %v", expiresAt, err)
	}
	if delta := parsed.Unix() - exp; delta < -1 || delta > 1 {
		t.Errorf("access_token_expires_at = %d, want %d", parsed.Unix(), exp)
	}
}

// 非 JWT 的 access_token（session key 形态）没有 exp：派生字段必须是「未知」而不是 0 秒。
func TestAccountDetailNonJWTTokenReportsUnknownExpiry(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "opaque-session-token-not-a-jwt", "type": "free", "status": StatusNormal},
	})

	detail := svc.Detail("opaque-session-token-not-a-jwt")
	if detail == nil {
		t.Fatalf("Detail returned nil")
	}

	if got := stringValue(detail["access_token_expires_at"]); got != "" {
		t.Errorf("access_token_expires_at = %q, want empty for a non-JWT token", got)
	}
	// 用 -1 表示「未知」，与「已过期（负数）」区分开。
	if got := intValue(detail["access_token_expires_in"]); got != -1 {
		t.Errorf("access_token_expires_in = %d, want -1 (unknown) for a non-JWT token", got)
	}
}

// 代理来源必须说清楚：账号自带 vs 回退全局 vs 直连。
// 否则运维看到代理地址无法判断「这个号是不是真的走它」。
func TestAccountDetailReportsProxySource(t *testing.T) {
	t.Run("account level", func(t *testing.T) {
		svc, _ := newTestAccountService(t, []map[string]any{
			{"access_token": "tok-own", "type": "free", "status": StatusNormal,
				"proxy": "socks5h://10.0.0.9:1080"},
		})
		detail := svc.Detail("tok-own")
		if got := stringValue(detail["proxy_source"]); got != "account" {
			t.Errorf("proxy_source = %q, want account", got)
		}
		if got := stringValue(detail["proxy_effective"]); got != "socks5h://10.0.0.9:1080" {
			t.Errorf("proxy_effective = %q", got)
		}
	})

	t.Run("global fallback", func(t *testing.T) {
		svc, cfg := newTestAccountService(t, []map[string]any{
			{"access_token": "tok-global", "type": "free", "status": StatusNormal},
		})
		if _, err := cfg.Update(map[string]any{"proxy": "http://global-proxy:8080"}); err != nil {
			t.Fatalf("update config proxy: %v", err)
		}
		detail := svc.Detail("tok-global")
		if got := stringValue(detail["proxy_source"]); got != "global" {
			t.Errorf("proxy_source = %q, want global", got)
		}
		if got := stringValue(detail["proxy_effective"]); got != "http://global-proxy:8080" {
			t.Errorf("proxy_effective = %q", got)
		}
	})

	t.Run("direct", func(t *testing.T) {
		svc, _ := newTestAccountService(t, []map[string]any{
			{"access_token": "tok-direct", "type": "free", "status": StatusNormal},
		})
		detail := svc.Detail("tok-direct")
		if got := stringValue(detail["proxy_source"]); got != "direct" {
			t.Errorf("proxy_source = %q, want direct", got)
		}
	})
}

// 不存在的账号返回 nil，由 handler 转 404。
func TestAccountDetailMissingReturnsNil(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)
	if detail := svc.Detail("does-not-exist"); detail != nil {
		t.Fatalf("Detail returned %v for a missing account", detail)
	}
	if detail := svc.Detail("   "); detail != nil {
		t.Fatalf("Detail returned %v for an empty token", detail)
	}
}

// 别名链：旧 token 必须能查到当前账号（与 GetAccount 语义一致）。
func TestAccountDetailResolvesAlias(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-current", "type": "free", "status": StatusNormal,
			"email": "alias@example.com"},
	})

	detail := svc.Detail("tok-current")
	if detail == nil {
		t.Fatalf("Detail returned nil")
	}
	if got := stringValue(detail["email"]); got != "alias@example.com" {
		t.Errorf("email = %q", got)
	}
	// 返回值必须是副本：调用方改它不能污染号池。
	detail["email"] = "mutated"
	if again := svc.Detail("tok-current"); stringValue(again["email"]) != "alias@example.com" {
		t.Errorf("Detail returned a shared reference; mutating it corrupted the pool")
	}
}

// 凭据存在性标记，供前端渲染「已配置 / 未配置」。
func TestAccountDetailCredentialFlags(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{
			"access_token":  "tok-full",
			"type":          "free",
			"status":        StatusNormal,
			"password":      "pw",
			"totp_secret":   "JBSWY3DPEHPK3PXP",
			"refresh_token": "rt",
		},
		{"access_token": "tok-bare", "type": "free", "status": StatusNormal},
	})

	full := svc.Detail("tok-full")
	for _, flag := range []string{
		"has_password", "has_totp_secret", "has_refresh_token",
	} {
		if value, ok := full[flag].(bool); !ok || !value {
			t.Errorf("%s = %v, want true", flag, full[flag])
		}
	}
	if value, ok := full["has_id_token"].(bool); !ok || value {
		t.Errorf("has_id_token = %v, want false", full["has_id_token"])
	}

	bare := svc.Detail("tok-bare")
	for _, flag := range []string{
		"has_password", "has_totp_secret", "has_refresh_token", "has_id_token",
	} {
		if value, ok := bare[flag].(bool); !ok || value {
			t.Errorf("%s = %v, want false", flag, bare[flag])
		}
	}
}

// AT 剩余有效期必须真的算出来，不能恒为 -1。
//
// 回归点：详情弹窗要显示「AT 剩余有效期」，这是运维判断「这个号还能用多久」
// 的唯一依据。若 tokenExpiresIn 对 JWT 也返回 nil（例如解析路径写错），
// 前端会一律显示「未知」，看起来像功能没做。
func TestDetailComputesAccessTokenExpiry(t *testing.T) {
	future := time.Now().Add(72 * time.Hour).Unix()
	token := testJWT(future)

	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": token, "type": "free", "status": StatusNormal},
	})

	detail := svc.Detail(token)
	if detail == nil {
		t.Fatal("详情为空")
	}
	remaining, ok := detail["access_token_expires_in"].(int)
	if !ok {
		t.Fatalf("access_token_expires_in 类型不对: %T", detail["access_token_expires_in"])
	}
	if remaining < 259000 || remaining > 259300 {
		t.Errorf("剩余 %d 秒，期望约 259200（72 小时）", remaining)
	}
	if at, _ := detail["access_token_expires_at"].(string); at == "" {
		t.Error("access_token_expires_at 为空，前端无法显示绝对时间")
	}
}

// 非 JWT 的 token 必须返回 -1（未知），不能返回 0。
//
// 0 与「已过期」在语义上无法区分，前端会显示「已过期」误导运维去重登
// 一个其实好用的号。
func TestDetailUnknownExpiryForNonJWT(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "not-a-jwt-session-key", "type": "free", "status": StatusNormal},
	})
	detail := svc.Detail("not-a-jwt-session-key")
	if detail == nil {
		t.Fatal("详情为空")
	}
	if got := detail["access_token_expires_in"]; got != -1 {
		t.Errorf("access_token_expires_in = %v，非 JWT 应为 -1", got)
	}
}

// testJWT 造一个只有 exp/iat 的 JWT（无签名校验，够 tokenExpiresIn 用）。
func testJWT(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"exp": exp, "iat": exp - 864000})
	body := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + body + ".sig"
}
