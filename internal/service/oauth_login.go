package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// OAuth 常量，逐字取自 services/openai_oauth.py。
const (
	oauthAuthBase          = "https://auth.openai.com"
	oauthPlatformBase      = "https://platform.openai.com"
	oauthRedirectURI       = "https://platform.openai.com/auth/callback"
	oauthAudience          = "https://api.openai.com/v1"
	oauthAuth0Client       = "eyJuYW1lIjoiYXV0aDAtc3BhLWpzIiwidmVyc2lvbiI6IjEuMjEuMCJ9"
	oauthSecChUa           = `"Google Chrome";v="145", "Not?A_Brand";v="8", "Chromium";v="145"`
	oauthSessionTTLSeconds = 10 * 60
	oauthMaxSessions       = 64
)

// OAuthLoginError 是 OAuth 桥流程的可预期错误（API 层翻成 400）。
type OAuthLoginError struct{ Message string }

func (e *OAuthLoginError) Error() string { return e.Message }

func newOAuthError(format string, args ...any) *OAuthLoginError {
	return &OAuthLoginError{Message: fmt.Sprintf(format, args...)}
}

// oauthSession 是一个待完成的 PKCE 会话。
type oauthSession struct {
	CodeVerifier string
	State        string
	CreatedAt    time.Time
	RedirectURI  string
}

// OAuthLoginService 复刻 services/oauth_login_service.py。
//
// 流程：生成 PKCE → 用户在浏览器授权 → 回填 callback → 用 verifier 换 token 三件套。
type OAuthLoginService struct {
	mu       sync.Mutex
	sessions map[string]oauthSession
}

// NewOAuthLoginService 构造。
func NewOAuthLoginService() *OAuthLoginService {
	return &OAuthLoginService{sessions: map[string]oauthSession{}}
}

// GeneratePKCE 复刻 utils/pkce.py 的 generate_pkce：
// verifier 是 64 字节随机数的 base64url（去 padding），challenge 是它的 SHA256。
func GeneratePKCE() (verifier, challenge string) {
	raw := make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		raw = make([]byte, 64)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

// purgeExpiredLocked 清理过期会话与溢出容量，必须在持锁时调用。
func (s *OAuthLoginService) purgeExpiredLocked() {
	now := time.Now()
	for id, session := range s.sessions {
		if now.Sub(session.CreatedAt) > oauthSessionTTLSeconds*time.Second {
			delete(s.sessions, id)
		}
	}
	if len(s.sessions) > oauthMaxSessions {
		type entry struct {
			id      string
			created time.Time
		}
		entries := make([]entry, 0, len(s.sessions))
		for id, session := range s.sessions {
			entries = append(entries, entry{id: id, created: session.CreatedAt})
		}
		sort.SliceStable(entries, func(i, j int) bool {
			return entries[i].created.Before(entries[j].created)
		})
		for _, item := range entries[:len(entries)-oauthMaxSessions] {
			delete(s.sessions, item.id)
		}
	}
}

// Start 登记一个 PKCE 会话，返回 session_id 与 authorize_url。
//
// state 形如 "<session_id>.<nonce>"：把 session_id 编进 state，
// 这样 finish 时即使前端状态丢了，也能从 callback URL 里恢复。
func (s *OAuthLoginService) Start(emailHint string) (map[string]string, error) {
	verifier, challenge := GeneratePKCE()
	nonce := randomURLSafe(32)
	deviceID := newUUIDValue()
	sessionID := strings.ReplaceAll(newUUIDValue(), "-", "")
	state := sessionID + "." + randomURLSafe(16)

	params := map[string]string{
		"issuer":                oauthAuthBase,
		"client_id":             oauthClientIDConst,
		"audience":              oauthAudience,
		"redirect_uri":          oauthRedirectURI,
		"device_id":             deviceID,
		"screen_hint":           "login_or_signup",
		"max_age":               "0",
		"scope":                 "openid profile email offline_access",
		"response_type":         "code",
		"response_mode":         "query",
		"state":                 state,
		"nonce":                 nonce,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"auth0Client":           oauthAuth0Client,
	}
	if hint := strings.TrimSpace(emailHint); hint != "" {
		params["login_hint"] = hint
	}

	authorizeURL := oauthAuthBase + "/api/accounts/authorize?" + encodeParams(params)

	s.mu.Lock()
	s.purgeExpiredLocked()
	s.sessions[sessionID] = oauthSession{
		CodeVerifier: verifier,
		State:        state,
		CreatedAt:    time.Now(),
		RedirectURI:  oauthRedirectURI,
	}
	s.mu.Unlock()

	return map[string]string{
		"session_id":          sessionID,
		"authorize_url":       authorizeURL,
		"expires_in":          fmt.Sprintf("%d", oauthSessionTTLSeconds),
		"redirect_uri_prefix": oauthRedirectURI,
	}, nil
}

// encodeParams 按 key 排序拼 query（保证 URL 可复现）。
func encodeParams(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(params[key]))
	}
	return strings.Join(parts, "&")
}

// ExtractCodeFromCallback 复刻 _extract_code_from_callback：
// 既接受完整 callback URL，也接受裸 code。
func ExtractCodeFromCallback(value string) (code, state string, err error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", "", nil
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		parsed, parseErr := url.Parse(raw)
		if parseErr != nil {
			return "", "", newOAuthError("无法解析 callback URL: %v", parseErr)
		}
		query := parsed.Query()
		code = strings.TrimSpace(query.Get("code"))
		state = strings.TrimSpace(query.Get("state"))
		if code == "" {
			detail := strings.TrimSpace(query.Get("error_description"))
			if detail == "" {
				detail = strings.TrimSpace(query.Get("error"))
			}
			if detail == "" {
				detail = "callback URL 中没有 code 参数"
			}
			return "", "", newOAuthError("%s", detail)
		}
		return code, state, nil
	}
	return raw, "", nil
}

// Finish 用 session 里的 verifier 把 code 换成 token 三件套。
//
// 关键行为（与 Python 一致）：
//   - state 里嵌的 session_id 优先于前端传来的
//   - state 不匹配要报明确错误（常见原因：用户开了两个授权页）
//   - 只有成功兑换才销毁 session，失败保留以便重试
func (s *OAuthLoginService) Finish(ctx CtxLike, sessionID, callback string) (map[string]string, error) {
	bodySessionID := strings.TrimSpace(sessionID)
	code, state, err := ExtractCodeFromCallback(callback)
	if err != nil {
		return nil, err
	}
	if code == "" {
		return nil, newOAuthError("缺少 code 或 callback URL")
	}

	stateSessionID := ""
	if state != "" {
		stateSessionID = strings.SplitN(state, ".", 2)[0]
	}
	candidates := make([]string, 0, 2)
	if stateSessionID != "" {
		candidates = append(candidates, stateSessionID)
	}
	if bodySessionID != "" {
		candidates = append(candidates, bodySessionID)
	}
	if len(candidates) == 0 {
		return nil, newOAuthError("既未提供 session_id，callback URL 中也未携带 state")
	}

	s.mu.Lock()
	s.purgeExpiredLocked()
	var session oauthSession
	found := false
	pickedID := ""
	for _, candidate := range candidates {
		if existing, ok := s.sessions[candidate]; ok {
			session = existing
			pickedID = candidate
			found = true
			break
		}
	}
	s.mu.Unlock()

	if !found {
		return nil, newOAuthError("OAuth 会话已过期或不存在，请回到导入对话框点\"重新生成\"再走一次")
	}
	if state != "" && session.State != "" && state != session.State {
		return nil, newOAuthError(
			"state 不匹配。常见原因：你点过两次\"打开授权页面\"，但浏览器里登录的还是前一次的窗口。请点\"重新生成\"重来。")
	}

	tokens, exchangeErr := exchangeOAuthCode(ctx, code, session.CodeVerifier, session.RedirectURI)
	if exchangeErr != nil {
		return nil, exchangeErr
	}

	// 仅成功后才消耗 session。
	s.mu.Lock()
	delete(s.sessions, pickedID)
	s.mu.Unlock()
	return tokens, nil
}

// exchangeOAuthCode 复刻 _exchange_code：POST /api/accounts/oauth/token。
func exchangeOAuthCode(ctx CtxLike, code, codeVerifier, redirectURI string) (map[string]string, error) {
	payload, err := json.Marshal(map[string]any{
		"client_id":     oauthClientIDConst,
		"code_verifier": codeVerifier,
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  redirectURI,
	})
	if err != nil {
		return nil, err
	}

	headers := map[string]string{
		"accept":             "application/json",
		"accept-language":    "en-US,en;q=0.9",
		"content-type":       "application/json",
		"origin":             oauthPlatformBase,
		"referer":            oauthPlatformBase + "/",
		"auth0-client":       oauthAuth0Client,
		"sec-ch-ua":          oauthSecChUa,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "same-origin",
		"user-agent":         oauthUserAgentConst,
	}
	target := oauthAuthBase + "/api/accounts/oauth/token"
	body, status, err := httpPostJSON(ctx, target, headers, payload, 60*time.Second)
	if err != nil {
		return nil, newOAuthError("换 token 网络异常: %v", err)
	}

	var data map[string]any
	_ = json.Unmarshal(body, &data)
	if status != 200 || data == nil || strings.TrimSpace(stringValue(data["access_token"])) == "" {
		detail := ""
		if data != nil {
			detail = strings.TrimSpace(stringValue(data["error_description"]))
			if detail == "" {
				detail = strings.TrimSpace(stringValue(data["error"]))
			}
			if detail == "" {
				detail = strings.TrimSpace(stringValue(data["message"]))
			}
		}
		if detail == "" && len(body) > 0 {
			truncated := string(body)
			if len(truncated) > 300 {
				truncated = truncated[:300]
			}
			detail = truncated
		}
		if detail != "" {
			return nil, newOAuthError("OpenAI 拒绝换 token (HTTP %d): %s", status, detail)
		}
		return nil, newOAuthError("OpenAI 拒绝换 token (HTTP %d)", status)
	}

	accessToken := strings.TrimSpace(stringValue(data["access_token"]))
	refreshToken := strings.TrimSpace(stringValue(data["refresh_token"]))
	idToken := strings.TrimSpace(stringValue(data["id_token"]))

	if accessToken == "" {
		return nil, newOAuthError("OpenAI 返回的 access_token 为空")
	}
	if refreshToken == "" {
		return nil, newOAuthError(
			"OpenAI 没有返回 refresh_token（可能 scope 未包含 offline_access 或 code 已使用过）")
	}
	return map[string]string{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"id_token":      idToken,
	}, nil
}

// randomURLSafe 生成 base64url 随机串（去 padding），对应 secrets.token_urlsafe。
func randomURLSafe(byteLen int) string {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", byteLen)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// newUUIDValue 生成带连字符的 uuid4。
func newUUIDValue() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	encoded := randomHex(16)
	_ = encoded
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 36)
	pos := 0
	for index, value := range buf {
		if index == 4 || index == 6 || index == 8 || index == 10 {
			out[pos] = '-'
			pos++
		}
		out[pos] = hexDigits[value>>4]
		out[pos+1] = hexDigits[value&0x0f]
		pos += 2
	}
	return string(out)
}

// ErrOAuthSessionMissing 供调用方判别。
var ErrOAuthSessionMissing = errors.New("oauth session missing")
