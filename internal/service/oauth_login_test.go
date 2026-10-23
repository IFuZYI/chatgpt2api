package service

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

func TestGeneratePKCE(t *testing.T) {
	verifier, challenge := GeneratePKCE()
	if len(verifier) < 43 {
		t.Errorf("verifier too short: %d chars", len(verifier))
	}
	if strings.ContainsAny(verifier, "+/=") {
		t.Errorf("verifier must be base64url without padding: %q", verifier)
	}
	if len(challenge) != 43 {
		t.Errorf("challenge should be 43 chars (sha256 base64url), got %d", len(challenge))
	}
	// 两次生成必须不同（随机性）
	verifier2, _ := GeneratePKCE()
	if verifier == verifier2 {
		t.Error("PKCE verifier must be random")
	}
}

func TestOAuthStartBuildsAuthorizeURL(t *testing.T) {
	service := NewOAuthLoginService()
	result, err := service.Start("user@example.com")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if result["session_id"] == "" {
		t.Error("session_id missing")
	}
	if result["expires_in"] != "600" {
		t.Errorf("expires_in = %q, want 600", result["expires_in"])
	}
	if result["redirect_uri_prefix"] != oauthRedirectURI {
		t.Errorf("redirect_uri_prefix = %q", result["redirect_uri_prefix"])
	}

	parsed, err := url.Parse(result["authorize_url"])
	if err != nil {
		t.Fatalf("authorize_url invalid: %v", err)
	}
	if !strings.HasPrefix(parsed.String(), "https://auth.openai.com/api/accounts/authorize?") {
		t.Errorf("wrong authorize endpoint: %s", parsed.String())
	}
	query := parsed.Query()
	checks := map[string]string{
		"client_id":             oauthClientIDConst,
		"audience":              oauthAudience,
		"redirect_uri":          oauthRedirectURI,
		"response_type":         "code",
		"response_mode":         "query",
		"code_challenge_method": "S256",
		"scope":                 "openid profile email offline_access",
		"login_hint":            "user@example.com",
		"screen_hint":           "login_or_signup",
	}
	for key, want := range checks {
		if got := query.Get(key); got != want {
			t.Errorf("query %s = %q, want %q", key, got, want)
		}
	}
	// state 必须含 session_id（finish 时用来恢复）
	state := query.Get("state")
	if !strings.HasPrefix(state, result["session_id"]+".") {
		t.Errorf("state should start with session_id: %q", state)
	}
	if query.Get("code_challenge") == "" {
		t.Error("code_challenge missing")
	}
}

func TestOAuthStartWithoutEmailHint(t *testing.T) {
	service := NewOAuthLoginService()
	result, err := service.Start("")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	parsed, _ := url.Parse(result["authorize_url"])
	if parsed.Query().Get("login_hint") != "" {
		t.Error("login_hint should be absent when email_hint is empty")
	}
}

func TestExtractCodeFromCallback(t *testing.T) {
	// 完整 URL
	code, state, err := ExtractCodeFromCallback("https://platform.openai.com/auth/callback?code=abc123&state=sess.nonce")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != "abc123" || state != "sess.nonce" {
		t.Errorf("code=%q state=%q", code, state)
	}

	// 裸 code
	code, state, err = ExtractCodeFromCallback("rawcode")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != "rawcode" || state != "" {
		t.Errorf("bare code: code=%q state=%q", code, state)
	}

	// 空
	code, state, err = ExtractCodeFromCallback("")
	if err != nil || code != "" || state != "" {
		t.Errorf("empty should be empty: code=%q state=%q err=%v", code, state, err)
	}

	// URL 里没有 code 但有 error
	_, _, err = ExtractCodeFromCallback("https://x/cb?error=access_denied&error_description=user+denied")
	if err == nil {
		t.Fatal("missing code with error should fail")
	}
	if !strings.Contains(err.Error(), "user denied") {
		t.Errorf("error should surface description: %v", err)
	}
}

func TestOAuthFinishRejectsUnknownSession(t *testing.T) {
	service := NewOAuthLoginService()
	_, err := service.Finish(context.Background(), "nope", "https://x/cb?code=abc")
	if err == nil {
		t.Fatal("unknown session should fail")
	}
	if !strings.Contains(err.Error(), "过期或不存在") {
		t.Errorf("error should mention expiry: %v", err)
	}
}

func TestOAuthFinishRequiresCode(t *testing.T) {
	service := NewOAuthLoginService()
	_, err := service.Finish(context.Background(), "sess", "")
	if err == nil {
		t.Fatal("missing code should fail")
	}
	if !strings.Contains(err.Error(), "缺少 code") {
		t.Errorf("error = %v", err)
	}
}

// state 不匹配必须给出明确提示（常见原因：开了两个授权页）。
func TestOAuthFinishStateMismatch(t *testing.T) {
	service := NewOAuthLoginService()
	result, _ := service.Start("")
	sessionID := result["session_id"]

	parsed, _ := url.Parse(result["authorize_url"])
	realState := parsed.Query().Get("state")

	// 用错的 state
	_, err := service.Finish(context.Background(), sessionID,
		"https://x/cb?code=abc&state=wrong.nonce")
	if err == nil {
		t.Fatal("state mismatch should fail")
	}
	if !strings.Contains(err.Error(), "state 不匹配") {
		t.Errorf("error = %v", err)
	}
	_ = realState
}

// 会话容量上限要生效（防止内存无限增长）。
//
// 注意稳态是 oauthMaxSessions+1，不是 oauthMaxSessions：清理发生在插入**之前**，
// 所以 64 个时清理不动作、插入后变 65；下次 Start 时清理掉 1 个再插回 65。
// 这是 Python 的既有行为（_purge_expired_locked 也在赋值前调用），刻意保持一致。
func TestOAuthSessionCap(t *testing.T) {
	service := NewOAuthLoginService()
	for i := 0; i < oauthMaxSessions+20; i++ {
		if _, err := service.Start(""); err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
	}
	service.mu.Lock()
	count := len(service.sessions)
	service.mu.Unlock()
	if count > oauthMaxSessions+1 {
		t.Errorf("sessions = %d, want <= %d (cap+1 because purge runs before insert)",
			count, oauthMaxSessions+1)
	}
	if count < oauthMaxSessions {
		t.Errorf("sessions = %d, cap should keep it near %d", count, oauthMaxSessions)
	}
}
