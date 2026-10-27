package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// 本文件实现「邮箱 + 密码 + TOTP」完整登录链路。
//
// 与 Python 的差异：Python 的 _login_with_password 遇到 mfa_challenge 直接放弃
// （返回 need_verification_code）。这里补上第 ⑤ 步 mfa/verify，用 RFC 6238 TOTP
// 完成两步验证，因此拿到 code 后可以正常换 token。
//
// 链路（全部经真实上游实测，见 .superpowers/sdd/plan-verify/task-password-login-brief.md）：
//
//	① GET  /api/accounts/authorize?<PKCE + login_hint=email>   → 200 或 302
//	② POST sentinel.openai.com/backend-api/sentinel/req        → challenge + PoW
//	③ POST /api/accounts/password/verify (需 sentinel + oai-device-id)
//	④ 生成 TOTP（SHA1 / 6 位 / 30 秒）
//	⑤ POST /api/accounts/mfa/verify  {"type":"totp","id":<factor_id>,"code":...}
//	⑥ POST /api/accounts/oauth/token 换 token 三件套（含 refresh_token）
const (
	passwordLoginSentinelFlow = "password_verify"
	passwordLoginReferer      = oauthAuthBase + "/log-in/password"
	totpPeriodSeconds         = 30
	totpDigits                = 6
)

// 密码登录的错误码。这些字面量会写进进度接口的 error 字段，前端与日志都按它分类。
const (
	// PasswordLoginNeedTOTP 表示账号开了 TOTP，但本地没有 totp_secret。
	PasswordLoginNeedTOTP = "need_totp"
	// PasswordLoginNeedVerificationCode 表示上游要邮箱验证码（不是 TOTP）。
	PasswordLoginNeedVerificationCode = "need_verification_code"
	// PasswordLoginNoAuthCode 表示流程走完了但没有拿到 authorization code。
	PasswordLoginNoAuthCode = "no_auth_code"
	// PasswordLoginInvalidPassword 表示密码错误。
	PasswordLoginInvalidPassword = "invalid_password"
	// PasswordLoginInvalidUsernameOrPassword 是上游对凭据错误的另一种 code。
	//
	// 实测（2026-10-27）：password/verify 返回
	//   HTTP 401 {"code":"invalid_username_or_password",
	//             "message":"Login failed. Please try again or seek help at ..."}
	// message 是通用文案，不含 "invalid credentials" / "wrong password"，
	// 只按 message 匹配会漏判成 password_verify_failed_401 —— 于是
	// 一个密码确实错掉的账号被当成"环境问题"无限重试。
	PasswordLoginInvalidUsernameOrPassword = "invalid_username_or_password"
	// PasswordLoginInvalidState 表示 authorize 的 state 失效。
	PasswordLoginInvalidState = "invalid_state"
	// PasswordLoginUnsupportedRegion 表示出口 IP 所在地区不被支持。
	PasswordLoginUnsupportedRegion = "unsupported_country_region_territory"
	// PasswordLoginRateLimited 表示触发了上游限流。
	PasswordLoginRateLimited = "rate_limit_exceeded"
	// PasswordLoginAccountDeactivated 表示账号被停用/删除。
	PasswordLoginAccountDeactivated = "account_deactivated"
	// PasswordLoginNoPassword 表示账号里没有密码（跳过而不是静默）。
	PasswordLoginNoPassword = "no_password_in_account"
	// PasswordLoginNoEmail 表示账号里没有邮箱（无法定位登录目标）。
	PasswordLoginNoEmail = "no_email_in_account"
)

// PasswordLoginError 是密码登录的可预期失败：Code 用于分类，Message 给人看。
type PasswordLoginError struct {
	Code    string
	Message string
	Detail  map[string]any
}

func (e *PasswordLoginError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func newPasswordLoginError(code, format string, args ...any) *PasswordLoginError {
	return &PasswordLoginError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// PasswordLoginTransport 是密码登录需要的上游能力（*upstream.Client 实现它）。
//
// 之所以在 service 侧定义接口而不是直接吃 *upstream.Client：测试要用
// httptest 假上游，不能把 service 焊死在 tls-client 上。
type PasswordLoginTransport interface {
	// AuthGet 发认证链路 GET，返回 body / 状态码 / 跟随重定向后的最终 URL。
	AuthGet(ctx context.Context, target string, headers map[string]string, timeout time.Duration) ([]byte, int, string, error)
	// AuthPost 发认证链路 POST。
	AuthPost(ctx context.Context, target string, headers map[string]string, payload []byte, timeout time.Duration) ([]byte, int, error)
	// BuildSentinelToken 返回 (openai-sentinel-token, oai-sc cookie)。
	BuildSentinelToken(ctx context.Context, deviceID, flow string) (string, string, error)
	// SetSessionCookie 往会话里塞 cookie（domain 可带前导点）。
	SetSessionCookie(domain, name, value string)
	// Close 释放连接。
	Close()
}

// PasswordLoginClientFactory 构造一个 transport；每次登录一个新会话。
//
// proxyURL 是该账号的出口代理（可能为空 = 直连）。工厂必须把它传给
// 上游客户端，不能忽略 —— 否则账号在重登时从另一个 IP 出去。
type PasswordLoginClientFactory func(proxyURL string) (PasswordLoginTransport, error)

// GenerateTOTP 生成 RFC 6238 的 6 位验证码（SHA1 / 30 秒窗口）。
//
// secret 是 base32（大小写不敏感，允许空格与 '=' 填充）。
func GenerateTOTP(secret string, at time.Time) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	return hotpCode(key, uint64(at.Unix()/totpPeriodSeconds), totpDigits)
}

// decodeTOTPSecret 解码 base32 secret。Google Authenticator 导出的串通常无填充，
// 但用户手工粘贴的经常带 '=' 或空格，两种都要接受。
func decodeTOTPSecret(secret string) ([]byte, error) {
	normalized := strings.ToUpper(strings.TrimSpace(secret))
	normalized = strings.ReplaceAll(normalized, " ", "")
	normalized = strings.ReplaceAll(normalized, "-", "")
	normalized = strings.TrimRight(normalized, "=")
	if normalized == "" {
		return nil, fmt.Errorf("totp secret is empty")
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(normalized)
	if err != nil {
		return nil, fmt.Errorf("totp secret is not valid base32: %w", err)
	}
	return key, nil
}

// hotpCode 是 RFC 4226 的动态截断，digits 支持 6/8 位（RFC 6238 附录 B 用 8 位）。
func hotpCode(key []byte, counter uint64, digits int) (string, error) {
	if len(key) == 0 {
		return "", fmt.Errorf("totp key is empty")
	}
	if digits <= 0 || digits > 10 {
		return "", fmt.Errorf("unsupported totp digits: %d", digits)
	}
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(message[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	truncated := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	modulus := uint32(1)
	for i := 0; i < digits; i++ {
		modulus *= 10
	}
	return fmt.Sprintf("%0*d", digits, truncated%modulus), nil
}

// authorizeParams 拼 ① 的 query。字段集与 services/account_service.py 逐字对齐。
func authorizeParams(email, deviceID, state, nonce, challenge string) map[string]string {
	return map[string]string{
		"issuer":                oauthAuthBase,
		"client_id":             oauthClientIDConst,
		"audience":              oauthAudience,
		"redirect_uri":          oauthRedirectURI,
		"device_id":             deviceID,
		"screen_hint":           "login_or_signup",
		"max_age":               "0",
		"login_hint":            email,
		"scope":                 "openid profile email offline_access",
		"response_type":         "code",
		"response_mode":         "query",
		"state":                 state,
		"nonce":                 nonce,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"auth0Client":           oauthAuth0Client,
	}
}

// authorizeHeaders 是 ① 的请求头（Python 里 session.get 的 headers 参数）。
func authorizeHeaders() map[string]string {
	return map[string]string{
		"accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"accept-language":           "zh-CN,zh;q=0.9,en;q=0.8",
		"user-agent":                oauthUserAgentConst,
		"sec-ch-ua":                 oauthLoginSecChUa,
		"sec-ch-ua-mobile":          "?0",
		"sec-ch-ua-platform":        `"Windows"`,
		"sec-fetch-dest":            "document",
		"sec-fetch-mode":            "navigate",
		"sec-fetch-site":            "cross-site",
		"sec-fetch-user":            "?1",
		"upgrade-insecure-requests": "1",
		"referer":                   oauthPlatformBase + "/",
	}
}

// oauthLoginSecChUa 是密码登录链路的 sec-ch-ua（Python 写死 Chrome 145 那一串）。
const oauthLoginSecChUa = `"Chromium";v="145", "Google Chrome";v="145", "Not/A)Brand";v="99"`

// passwordVerifyHeaders 是 ③ 的请求头。
func passwordVerifyHeaders(deviceID, sentinelToken string) map[string]string {
	headers := map[string]string{
		"accept":             "application/json",
		"accept-language":    "zh-CN,zh;q=0.9",
		"content-type":       "application/json",
		"origin":             oauthAuthBase,
		"priority":           "u=1, i",
		"user-agent":         oauthUserAgentConst,
		"sec-ch-ua":          oauthLoginSecChUa,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "same-origin",
		"referer":            passwordLoginReferer,
		"oai-device-id":      deviceID,
	}
	if sentinelToken != "" {
		headers["openai-sentinel-token"] = sentinelToken
	}
	return headers
}

// mfaVerifyHeaders 是 ⑤ 的请求头。referer 必须是 ③ 返回的 continue_url。
func mfaVerifyHeaders(deviceID, referer string) map[string]string {
	return map[string]string{
		"accept":             "application/json",
		"content-type":       "application/json",
		"origin":             oauthAuthBase,
		"referer":            referer,
		"oai-device-id":      deviceID,
		"user-agent":         oauthUserAgentConst,
		"sec-ch-ua":          oauthLoginSecChUa,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
	}
}

// LoginWithPassword 走完整的 ①→⑥ 密码登录链路。
//
// 返回 {access_token, refresh_token, id_token}；失败返回 *PasswordLoginError，
// 其中的 Code 是明确分类（绝不静默跳过）。
func LoginWithPassword(ctx CtxLike, newTransport PasswordLoginClientFactory, email, password, totpSecret string) (map[string]string, error) {
	return LoginWithPasswordVia(ctx, newTransport, "", email, password, totpSecret)
}

// LoginWithPasswordVia 是带出口代理的版本。
//
// proxyURL 由调用方从账号记录解析（AccountService.ProxyFor），
// 保持「同一账号的所有出站走同一个出口」这一不变量。
func LoginWithPasswordVia(
	ctx CtxLike,
	newTransport PasswordLoginClientFactory,
	proxyURL, email, password, totpSecret string,
) (map[string]string, error) {
	trimmedEmail := strings.TrimSpace(email)
	if trimmedEmail == "" {
		return nil, newPasswordLoginError(PasswordLoginNoEmail, "账号缺少 email，无法发起密码登录")
	}
	if strings.TrimSpace(password) == "" {
		return nil, newPasswordLoginError(PasswordLoginNoPassword, "账号缺少 password，无法发起密码登录")
	}
	if newTransport == nil {
		return nil, newPasswordLoginError("transport_not_configured", "密码登录的上游客户端未注入")
	}
	transport, err := newTransport(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("build auth client: %w", err)
	}
	defer transport.Close()

	deviceID := newUUIDValue()
	verifier, challenge := GeneratePKCE()
	state := randomURLSafe(32)
	nonce := randomURLSafe(32)

	// ── ① authorize ────────────────────────────────────────────────
	authorizeURL := oauthAuthBase + "/api/accounts/authorize?" + encodeParams(
		authorizeParams(trimmedEmail, deviceID, state, nonce, challenge))
	// Python 在发请求前把 oai-did 塞进 .auth.openai.com 的 cookie。
	transport.SetSessionCookie(".auth.openai.com", "oai-did", deviceID)

	body, status, finalURL, err := transport.AuthGet(ctx, authorizeURL, authorizeHeaders(), 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("authorize request failed: %w", err)
	}
	if status != 200 && status != 302 {
		return nil, newPasswordLoginError(fmt.Sprintf("authorize_failed_%d", status),
			"authorize 返回 HTTP %d: %s", status, truncateText(string(body), 300))
	}
	if code, payload := authorizeErrorCode(finalURL); code != "" {
		if code == PasswordLoginRateLimited {
			return nil, &PasswordLoginError{Code: PasswordLoginRateLimited, Message: "上游限流（rate_limit_exceeded）", Detail: payload}
		}
		return nil, &PasswordLoginError{Code: "authorize_error_" + code, Message: "authorize 重定向到错误页: " + code, Detail: payload}
	}

	// ── ② sentinel token（password_verify 流程）─────────────────────
	sentinelToken, oaiSCCookie, err := transport.BuildSentinelToken(ctx, deviceID, passwordLoginSentinelFlow)
	if err != nil {
		// Python 这里是 try/except pass，然后 password/verify 稳定 403。
		// 实测 password/verify 必须有 sentinel，所以把失败如实报出来。
		return nil, newPasswordLoginError("sentinel_failed", "获取 sentinel token 失败: %v", err)
	}
	if oaiSCCookie != "" {
		transport.SetSessionCookie(".openai.com", "oai-sc", oaiSCCookie)
	}

	// ── ③ password/verify ─────────────────────────────────────────
	passwordPayload, err := json.Marshal(map[string]any{"password": password})
	if err != nil {
		return nil, err
	}
	body, status, err = transport.AuthPost(ctx, oauthAuthBase+"/api/accounts/password/verify",
		passwordVerifyHeaders(deviceID, sentinelToken), passwordPayload, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("password/verify request failed: %w", err)
	}
	loginData := decodeJSONMap(body)
	if status != 200 {
		return nil, passwordVerifyError(status, loginData, body)
	}

	continueURL := strings.TrimSpace(stringValue(loginData["continue_url"]))
	pageType, pagePayload := pageInfo(loginData)
	authCode := extractQueryParam(continueURL, "code")

	// ── ④⑤ mfa_challenge → TOTP ──────────────────────────────────
	if authCode == "" && pageType == "mfa_challenge" {
		if strings.TrimSpace(totpSecret) == "" {
			return nil, &PasswordLoginError{
				Code:    PasswordLoginNeedTOTP,
				Message: "该账号开启了 TOTP 两步验证，但账号里没有 totp_secret",
				Detail:  map[string]any{"continue_url": continueURL, "page": loginData["page"]},
			}
		}
		factorID := mfaFactorID(pagePayload)
		if factorID == "" {
			return nil, &PasswordLoginError{
				Code:    PasswordLoginNoAuthCode,
				Message: "mfa_challenge 页面没有 factor_id，无法提交验证码",
				Detail:  map[string]any{"continue_url": continueURL, "page": loginData["page"]},
			}
		}
		totpCode, totpErr := GenerateTOTP(totpSecret, time.Now())
		if totpErr != nil {
			return nil, &PasswordLoginError{
				Code:    "totp_secret_invalid",
				Message: "totp_secret 无法生成验证码: " + totpErr.Error(),
				Detail:  map[string]any{"continue_url": continueURL},
			}
		}
		// 参数名是 id（不是 factor_id）—— 实测传 factor_id 会 400。
		// 用结构体保住 Python dict 的键序（type,id,code）。
		mfaPayload, marshalErr := json.Marshal(mfaVerifyPayload{
			Type: "totp",
			ID:   factorID,
			Code: totpCode,
		})
		if marshalErr != nil {
			return nil, marshalErr
		}
		body, status, err = transport.AuthPost(ctx, oauthAuthBase+"/api/accounts/mfa/verify",
			mfaVerifyHeaders(deviceID, continueURL), mfaPayload, 30*time.Second)
		if err != nil {
			return nil, fmt.Errorf("mfa/verify request failed: %w", err)
		}
		mfaData := decodeJSONMap(body)
		if status != 200 {
			return nil, mfaVerifyError(status, mfaData, body)
		}
		continueURL = strings.TrimSpace(stringValue(mfaData["continue_url"]))
		authCode = extractQueryParam(continueURL, "code")
		if authCode == "" {
			return nil, &PasswordLoginError{
				Code:    PasswordLoginNoAuthCode,
				Message: "mfa/verify 成功但没有返回 authorization code",
				Detail:  map[string]any{"continue_url": continueURL, "response": mfaData},
			}
		}
	}

	if authCode == "" {
		switch pageType {
		case "email_otp_verification":
			return nil, &PasswordLoginError{
				Code:    PasswordLoginNeedVerificationCode,
				Message: "该账号要求邮箱验证码，无法自动完成",
				Detail:  map[string]any{"continue_url": continueURL, "page": loginData["page"]},
			}
		default:
			return nil, &PasswordLoginError{
				Code:    PasswordLoginNoAuthCode,
				Message: "登录流程没有返回 authorization code（page.type=" + pageType + "）",
				Detail:  map[string]any{"continue_url": continueURL, "page": loginData["page"]},
			}
		}
	}

	// ── ⑥ code → token 三件套 ────────────────────────────────────
	tokens, exchangeErr := exchangeOAuthCodeVia(ctx, oauthAuthBase+"/api/accounts/oauth/token",
		authCode, verifier, oauthRedirectURI, transport.AuthPost)
	if exchangeErr != nil {
		return nil, &PasswordLoginError{Code: "token_exchange_failed", Message: exchangeErr.Error()}
	}
	return tokens, nil
}

// mfaVerifyPayload 是 ⑤ 的请求体：{"type":"totp","id":<factor_id>,"code":<6位>}。
type mfaVerifyPayload struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Code string `json:"code"`
}

// accountDeactivatedSignals 是「账号已被停用/删除」的判定信号。
//
// 为什么必须单独抽出来：上游把 account_deactivated 报在**多个步骤**上，
// 而每个步骤各有一套错误映射。实测（2026-10-27，真实封号账号）：
// 密码校验能过，账号在 **mfa/verify** 才回 403 + code=account_deactivated。
// 只有 passwordVerifyError 认这个 code 时，封号会被降级成
// mfa_verify_failed_403 → 上层判为「非确定性失败」→ 永远不禁用、永远重试。
//
// 判定同时看 code 与 message：上游不同步骤给的 code 未必一致，
// 但 message 里固定出现 "deleted or deactivated"。
func accountDeactivatedSignals(code, message string) bool {
	if strings.EqualFold(strings.TrimSpace(code), PasswordLoginAccountDeactivated) {
		return true
	}
	lowered := strings.ToLower(message)
	return strings.Contains(lowered, "deleted or deactivated") ||
		strings.Contains(lowered, "account_deactivated") ||
		strings.Contains(lowered, "account has been deleted") ||
		strings.Contains(lowered, "account is deactivated")
}

// passwordVerifyError 把 ③ 的非 200 响应映射成明确错误码。
func passwordVerifyError(status int, data map[string]any, body []byte) error {
	code, message := upstreamErrorFields(data)
	switch {
	case code == PasswordLoginUnsupportedRegion:
		return &PasswordLoginError{Code: PasswordLoginUnsupportedRegion, Message: "出口 IP 所在地区不被支持: " + message, Detail: data}
	case code == PasswordLoginInvalidState:
		return &PasswordLoginError{Code: PasswordLoginInvalidState, Message: "authorize 的 state 已失效，请重试: " + message, Detail: data}
	case code == PasswordLoginRateLimited:
		return &PasswordLoginError{Code: PasswordLoginRateLimited, Message: "上游限流: " + message, Detail: data}
	case accountDeactivatedSignals(code, message):
		return &PasswordLoginError{Code: PasswordLoginAccountDeactivated, Message: "账号已停用: " + message, Detail: data}
	case code == PasswordLoginInvalidPassword,
		code == PasswordLoginInvalidUsernameOrPassword,
		strings.Contains(strings.ToLower(message), "invalid credentials"),
		strings.Contains(strings.ToLower(message), "invalid username or password"),
		strings.Contains(strings.ToLower(message), "wrong password"):
		// 优先认 code：上游若改用别的措辞仍能命中。消息匹配是兜底
		// （实测上游走的是消息路径，code 未必出现）。
		//
		// invalid_username_or_password 是实测到的第二种凭据错误码，
		// 它的 message 是通用文案（"Login failed. Please try again..."），
		// 不含任何可用于匹配的关键词 —— 只按 message 判断会漏成通用 401。
		return &PasswordLoginError{Code: PasswordLoginInvalidPassword, Message: "密码错误: " + message, Detail: data}
	}
	detail := message
	if detail == "" {
		detail = truncateText(string(body), 300)
	}
	if code != "" {
		return &PasswordLoginError{
			Code:    fmt.Sprintf("password_verify_failed_%d", status),
			Message: fmt.Sprintf("password/verify 返回 HTTP %d（%s）: %s", status, code, detail),
			Detail:  data,
		}
	}
	return &PasswordLoginError{
		Code:    fmt.Sprintf("password_verify_failed_%d", status),
		Message: fmt.Sprintf("password/verify 返回 HTTP %d: %s", status, detail),
		Detail:  data,
	}
}

// mfaVerifyError 把 ⑤ 的非 200 响应映射成明确错误码。
//
// 关键：必须识别 account_deactivated。实测（2026-10-27）被封账号的
// 密码校验能通过，上游在 mfa/verify 才回 403 + code=account_deactivated。
// 早期实现只在这里做通用兜底（mfa_verify_failed_403），导致封号被判成
// 「非确定性失败」——不禁用、继续按 6 小时退避重试，永远出不来。
func mfaVerifyError(status int, data map[string]any, body []byte) error {
	code, message := upstreamErrorFields(data)
	if code == "" {
		code = truncateText(string(body), 120)
	}
	if message == "" {
		message = code
	}
	if accountDeactivatedSignals(code, message) {
		return &PasswordLoginError{
			Code:    PasswordLoginAccountDeactivated,
			Message: "账号已停用: " + message,
			Detail:  data,
		}
	}
	return &PasswordLoginError{
		Code:    fmt.Sprintf("mfa_verify_failed_%d", status),
		Message: fmt.Sprintf("mfa/verify 返回 HTTP %d: %s", status, message),
		Detail:  data,
	}
}

// upstreamErrorFields 取 {"error":{"code":..,"message":..}} 里的两个字段。
func upstreamErrorFields(data map[string]any) (code, message string) {
	if data == nil {
		return "", ""
	}
	if nested, ok := data["error"].(map[string]any); ok {
		return strings.TrimSpace(stringValue(nested["code"])), strings.TrimSpace(stringValue(nested["message"]))
	}
	return strings.TrimSpace(stringValue(data["error"])), strings.TrimSpace(stringValue(data["message"]))
}

// pageInfo 取响应里的 page.type 与 page.payload。
func pageInfo(data map[string]any) (string, map[string]any) {
	if data == nil {
		return "", nil
	}
	page, ok := data["page"].(map[string]any)
	if !ok {
		return "", nil
	}
	payload, _ := page["payload"].(map[string]any)
	return strings.TrimSpace(stringValue(page["type"])), payload
}

// mfaFactorID 从 page.payload 里取 factor_id，退回到 factors[0].id。
func mfaFactorID(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if factorID := strings.TrimSpace(stringValue(payload["factor_id"])); factorID != "" {
		return factorID
	}
	factors, ok := payload["factors"].([]any)
	if !ok {
		return ""
	}
	for _, item := range factors {
		factor, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if factorType := strings.TrimSpace(stringValue(factor["factor_type"])); factorType != "" && factorType != "totp" {
			continue
		}
		if factorID := strings.TrimSpace(stringValue(factor["id"])); factorID != "" {
			return factorID
		}
	}
	return ""
}

// extractQueryParam 从 URL 里取 query 参数（continue_url 可能是完整 URL 也可能是相对路径）。
func extractQueryParam(rawURL, key string) string {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Query().Get(key))
}

// authorizeErrorCode 复刻 Python 对 /error?payload=<base64> 的解析：
// 返回 errorCode 与解码后的 payload（解析失败返回空串，让调用方继续正常流程）。
func authorizeErrorCode(finalURL string) (string, map[string]any) {
	if !strings.Contains(finalURL, "/error") || !strings.Contains(finalURL, "payload=") {
		return "", nil
	}
	raw := extractQueryParam(finalURL, "payload")
	if raw == "" {
		return "", nil
	}
	if padding := len(raw) % 4; padding != 0 {
		raw += strings.Repeat("=", 4-padding)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", nil
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return "", nil
	}
	return strings.TrimSpace(stringValue(payload["errorCode"])), payload
}

// decodeJSONMap 解析响应体；坏 JSON 返回空 map（调用方按状态码兜底）。
func decodeJSONMap(body []byte) map[string]any {
	if len(body) == 0 {
		return map[string]any{}
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return map[string]any{}
	}
	if data == nil {
		return map[string]any{}
	}
	return data
}

// truncateText 截断过长的错误文本。
func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}
