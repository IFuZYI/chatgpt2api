package service

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ── TOTP ────────────────────────────────────────────────────────────

// RFC 6238 附录 B 的标准测试向量。
//
// secret 是 ASCII "12345678901234567890"（base32: GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ）。
// 附录 B 给的是 **8 位**结果，6 位是取其末 6 位（TOTP 的标准截断方式）。
func TestGenerateTOTPRFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	if secret != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" {
		t.Fatalf("unexpected base32 of RFC secret: %s", secret)
	}

	cases := []struct {
		unix  int64
		want8 string
		want6 string
	}{
		{59, "94287082", "287082"},
		{1111111109, "07081804", "081804"},
		{1111111111, "14050471", "050471"},
		{1234567890, "89005924", "005924"},
		{2000000000, "69279037", "279037"},
		{20000000000, "65353130", "353130"},
	}
	for _, item := range cases {
		at := time.Unix(item.unix, 0)
		got6, err := GenerateTOTP(secret, at)
		if err != nil {
			t.Fatalf("GenerateTOTP(%d): %v", item.unix, err)
		}
		if got6 != item.want6 {
			t.Errorf("T=%d: 6-digit = %q, want %q", item.unix, got6, item.want6)
		}
		key, err := decodeTOTPSecret(secret)
		if err != nil {
			t.Fatalf("decodeTOTPSecret: %v", err)
		}
		got8, err := hotpCode(key, uint64(item.unix)/totpPeriodSeconds, 8)
		if err != nil {
			t.Fatalf("hotpCode: %v", err)
		}
		if got8 != item.want8 {
			t.Errorf("T=%d: 8-digit = %q, want %q", item.unix, got8, item.want8)
		}
	}
}

// secret 的大小写、空格、'-' 与 '=' 填充都必须被接受（用户手工粘贴的常见形态）。
func TestGenerateTOTPAcceptsMessySecret(t *testing.T) {
	now := time.Unix(1700000000, 0)
	want, err := GenerateTOTP("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", now)
	if err != nil {
		t.Fatalf("GenerateTOTP: %v", err)
	}
	for _, messy := range []string{
		"gezdgnbvgy3tqojqgezdgnbvgy3tqojq",
		"GEZD GNBV GY3T QOJQ GEZD GNBV GY3T QOJQ",
		"GEZD-GNBV-GY3T-QOJQ-GEZD-GNBV-GY3T-QOJQ",
		"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ====",
		"  GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ\n",
	} {
		got, err := GenerateTOTP(messy, now)
		if err != nil {
			t.Fatalf("GenerateTOTP(%q): %v", messy, err)
		}
		if got != want {
			t.Errorf("GenerateTOTP(%q) = %q, want %q", messy, got, want)
		}
	}
}

// 非法 secret 必须报错（账号里塞了乱码时要给出明确原因）。
func TestGenerateTOTPRejectsInvalidSecret(t *testing.T) {
	for _, secret := range []string{"", "   ", "!!!!", "ABC"} {
		if _, err := GenerateTOTP(secret, time.Unix(59, 0)); err == nil {
			t.Errorf("GenerateTOTP(%q) should fail", secret)
		}
	}
}

// 同一 30 秒窗口内验证码必须相同，跨窗口必须变化（否则永远过不了）。
func TestGenerateTOTPWindowBoundary(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	first, _ := GenerateTOTP(secret, time.Unix(60, 0))
	last, _ := GenerateTOTP(secret, time.Unix(89, 0))
	if first != last {
		t.Errorf("codes within one 30s window must match: %q vs %q", first, last)
	}
	next, _ := GenerateTOTP(secret, time.Unix(90, 0))
	if next == first {
		t.Error("next window must produce a different code")
	}
}

// ── 完整链路（httptest 假上游）─────────────────────────────────────

// passwordAuthFake 是 PasswordLoginTransport 的测试替身：按顺序回放脚本，
// 并把每一次出站请求记下来供逐字段断言。
type passwordAuthFake struct {
	requests  []passwordAuthRequest
	responses map[string]passwordAuthResponse
	cookies   map[string]string
	closed    bool
	// proxyURL 记录工厂收到的出口代理，供「代理是否透传到底」的断言使用。
	proxyURL string
}

type passwordAuthRequest struct {
	kind    string // GET / POST
	target  string
	headers map[string]string
	body    []byte
}

type passwordAuthResponse struct {
	status   int
	body     string
	finalURL string
}

func newPasswordAuthFake() *passwordAuthFake {
	return &passwordAuthFake{
		responses: map[string]passwordAuthResponse{},
		cookies:   map[string]string{},
	}
}

func (f *passwordAuthFake) AuthGet(ctx context.Context, target string, headers map[string]string, timeout time.Duration) ([]byte, int, string, error) {
	f.requests = append(f.requests, passwordAuthRequest{kind: "GET", target: target, headers: headers})
	response := f.lookup(target)
	finalURL := response.finalURL
	if finalURL == "" {
		finalURL = target
	}
	return []byte(response.body), response.status, finalURL, nil
}

func (f *passwordAuthFake) AuthPost(ctx context.Context, target string, headers map[string]string, payload []byte, timeout time.Duration) ([]byte, int, error) {
	f.requests = append(f.requests, passwordAuthRequest{kind: "POST", target: target, headers: headers, body: payload})
	response := f.lookup(target)
	return []byte(response.body), response.status, nil
}

func (f *passwordAuthFake) BuildSentinelToken(ctx context.Context, deviceID, flow string) (string, string, error) {
	f.requests = append(f.requests, passwordAuthRequest{kind: "SENTINEL", target: flow})
	return `{"p":"requirements-token","t":"","c":"challenge-token","id":"` + deviceID + `","flow":"` + flow + `"}`,
		"0challenge-token", nil
}

func (f *passwordAuthFake) SetSessionCookie(domain, name, value string) {
	f.cookies[domain+"|"+name] = value
}

func (f *passwordAuthFake) Close() { f.closed = true }

func (f *passwordAuthFake) lookup(target string) passwordAuthResponse {
	for suffix, response := range f.responses {
		if strings.Contains(target, suffix) {
			return response
		}
	}
	return passwordAuthResponse{status: 404, body: `{"error":"unexpected target "}`}
}

func (f *passwordAuthFake) find(kind, contains string) *passwordAuthRequest {
	for index := range f.requests {
		request := &f.requests[index]
		if request.kind == kind && strings.Contains(request.target, contains) {
			return request
		}
	}
	return nil
}

func (f *passwordAuthFake) factory() PasswordLoginClientFactory {
	return func(proxyURL string) (PasswordLoginTransport, error) {
		f.proxyURL = proxyURL
		return f, nil
	}
}

// 全链路成功：authorize → sentinel → password/verify → mfa/verify → token。
// 每个请求的 body / 关键头逐字段断言（brief 里实测的形状）。
func TestLoginWithPasswordFullChain(t *testing.T) {
	const (
		email  = "user@example.com"
		secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	)
	fake := newPasswordAuthFake()
	fake.responses["/api/accounts/authorize"] = passwordAuthResponse{
		status:   200,
		body:     "<html>password page</html>",
		finalURL: "https://auth.openai.com/log-in/password",
	}
	fake.responses["/api/accounts/password/verify"] = passwordAuthResponse{
		status: 200,
		body: `{"continue_url":"https://auth.openai.com/mfa-challenge/factor-1",` +
			`"method":"GET","page":{"type":"mfa_challenge","payload":{"factor_id":"factor-1",` +
			`"factors":[{"id":"factor-1","factor_type":"totp","is_recovery":false}]}}}`,
	}
	fake.responses["/api/accounts/mfa/verify"] = passwordAuthResponse{
		status: 200,
		body:   `{"continue_url":"https://platform.openai.com/auth/callback?code=oaistb_ac_abc&scope=openid"}`,
	}
	fake.responses["/api/accounts/oauth/token"] = passwordAuthResponse{
		status: 200,
		body:   `{"access_token":"access-1","refresh_token":"refresh-1","id_token":"id-1"}`,
	}

	tokens, err := LoginWithPassword(context.Background(), fake.factory(), email, "pw-1", secret)
	if err != nil {
		t.Fatalf("LoginWithPassword: %v", err)
	}
	if tokens["access_token"] != "access-1" || tokens["refresh_token"] != "refresh-1" || tokens["id_token"] != "id-1" {
		t.Fatalf("tokens = %v", tokens)
	}
	if !fake.closed {
		t.Error("transport must be closed")
	}

	// ── ① authorize ─────────────────────────────────────────────
	authorize := fake.find("GET", "/api/accounts/authorize")
	if authorize == nil {
		t.Fatal("authorize request missing")
	}
	parsed, err := url.Parse(authorize.target)
	if err != nil {
		t.Fatalf("authorize url: %v", err)
	}
	if !strings.HasPrefix(authorize.target, oauthAuthBase+"/api/accounts/authorize?") {
		t.Errorf("authorize endpoint = %s", authorize.target)
	}
	query := parsed.Query()
	wantQuery := map[string]string{
		"client_id":             oauthClientIDConst,
		"audience":              oauthAudience,
		"redirect_uri":          oauthRedirectURI,
		"scope":                 "openid profile email offline_access",
		"response_type":         "code",
		"response_mode":         "query",
		"code_challenge_method": "S256",
		"screen_hint":           "login_or_signup",
		"max_age":               "0",
		"login_hint":            email,
		"auth0Client":           oauthAuth0Client,
	}
	for key, want := range wantQuery {
		if got := query.Get(key); got != want {
			t.Errorf("authorize query %s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"state", "nonce", "code_challenge", "device_id"} {
		if query.Get(key) == "" {
			t.Errorf("authorize query %s is empty", key)
		}
	}
	if got := authorize.headers["referer"]; got != "https://platform.openai.com/" {
		t.Errorf("authorize referer = %q", got)
	}
	if got := authorize.headers["sec-fetch-site"]; got != "cross-site" {
		t.Errorf("authorize sec-fetch-site = %q, want cross-site", got)
	}
	if got := fake.cookies[".auth.openai.com|oai-did"]; got == "" || got != query.Get("device_id") {
		t.Errorf("oai-did cookie = %q, want the authorize device_id %q", got, query.Get("device_id"))
	}

	// ── ② sentinel ──────────────────────────────────────────────
	sentinel := fake.find("SENTINEL", "")
	if sentinel == nil {
		t.Fatal("sentinel request missing")
	}
	if sentinel.target != passwordLoginSentinelFlow {
		t.Errorf("sentinel flow = %q, want %q", sentinel.target, passwordLoginSentinelFlow)
	}
	if got := fake.cookies[".openai.com|oai-sc"]; got != "0challenge-token" {
		t.Errorf("oai-sc cookie = %q, want 0challenge-token", got)
	}

	// ── ③ password/verify ───────────────────────────────────────
	verify := fake.find("POST", "/api/accounts/password/verify")
	if verify == nil {
		t.Fatal("password/verify request missing")
	}
	var verifyBody map[string]any
	if err := json.Unmarshal(verify.body, &verifyBody); err != nil {
		t.Fatalf("password/verify body not JSON: %v", err)
	}
	if len(verifyBody) != 1 || verifyBody["password"] != "pw-1" {
		t.Errorf("password/verify body = %s, want exactly {\"password\"}", verify.body)
	}
	if got := verify.headers["openai-sentinel-token"]; got == "" {
		t.Error("password/verify must carry openai-sentinel-token")
	}
	if got := verify.headers["oai-device-id"]; got != query.Get("device_id") {
		t.Errorf("oai-device-id = %q, want %q", got, query.Get("device_id"))
	}
	if got := verify.headers["referer"]; got != "https://auth.openai.com/log-in/password" {
		t.Errorf("password/verify referer = %q", got)
	}
	if got := verify.headers["origin"]; got != "https://auth.openai.com" {
		t.Errorf("password/verify origin = %q", got)
	}
	if got := verify.headers["content-type"]; got != "application/json" {
		t.Errorf("password/verify content-type = %q", got)
	}

	// ── ④⑤ mfa/verify ──────────────────────────────────────────
	mfa := fake.find("POST", "/api/accounts/mfa/verify")
	if mfa == nil {
		t.Fatal("mfa/verify request missing")
	}
	var mfaBody map[string]any
	if err := json.Unmarshal(mfa.body, &mfaBody); err != nil {
		t.Fatalf("mfa/verify body not JSON: %v", err)
	}
	// 参数名是 id（不是 factor_id）；type/id/code 三个字段，不多不少。
	if len(mfaBody) != 3 {
		t.Errorf("mfa/verify body keys = %v, want exactly type/id/code", mfaBody)
	}
	if mfaBody["type"] != "totp" {
		t.Errorf("mfa type = %v, want totp", mfaBody["type"])
	}
	if mfaBody["id"] != "factor-1" {
		t.Errorf("mfa id = %v, want factor-1", mfaBody["id"])
	}
	code, _ := mfaBody["code"].(string)
	if len(code) != 6 {
		t.Errorf("mfa code = %q, want 6 digits", code)
	}
	if _, ok := mfaBody["factor_id"]; ok {
		t.Error("mfa/verify must NOT send factor_id (upstream requires id)")
	}
	// 键序与 Python dict 一致（type,id,code）——用结构体而非 map 的原因。
	if order := jsonKeyOrder(t, mfa.body); strings.Join(order, ",") != "type,id,code" {
		t.Errorf("mfa/verify key order = %v, want [type id code]", order)
	}
	if got := mfa.headers["referer"]; got != "https://auth.openai.com/mfa-challenge/factor-1" {
		t.Errorf("mfa referer = %q, want the password/verify continue_url", got)
	}
	if _, ok := mfa.headers["openai-sentinel-token"]; ok {
		t.Error("mfa/verify does not need a sentinel token (verified: 200 without it)")
	}

	// code 必须与独立算出的 TOTP 一致（同一窗口内）。
	wantCode, totpErr := GenerateTOTP(secret, time.Now())
	if totpErr != nil {
		t.Fatalf("GenerateTOTP: %v", totpErr)
	}
	if code != wantCode {
		t.Errorf("mfa code = %q, want %q", code, wantCode)
	}

	// ── ⑥ token 交换 ────────────────────────────────────────────
	exchange := fake.find("POST", "/api/accounts/oauth/token")
	if exchange == nil {
		t.Fatal("oauth/token request missing")
	}
	var exchangeBody map[string]any
	if err := json.Unmarshal(exchange.body, &exchangeBody); err != nil {
		t.Fatalf("oauth/token body not JSON: %v", err)
	}
	if exchangeBody["code"] != "oaistb_ac_abc" {
		t.Errorf("exchange code = %v, want the code extracted from continue_url", exchangeBody["code"])
	}
	if exchangeBody["grant_type"] != "authorization_code" {
		t.Errorf("grant_type = %v", exchangeBody["grant_type"])
	}
	if exchangeBody["redirect_uri"] != oauthRedirectURI {
		t.Errorf("redirect_uri = %v", exchangeBody["redirect_uri"])
	}
	if verifier, _ := exchangeBody["code_verifier"].(string); verifier == "" {
		t.Error("code_verifier must be the PKCE verifier from step ①")
	}
}

// 没开 TOTP 的账号：password/verify 直接给 callback code，跳过 mfa。
func TestLoginWithPasswordWithoutMFA(t *testing.T) {
	fake := newPasswordAuthFake()
	fake.responses["/api/accounts/authorize"] = passwordAuthResponse{status: 200, body: "ok"}
	fake.responses["/api/accounts/password/verify"] = passwordAuthResponse{
		status: 200,
		body:   `{"continue_url":"https://platform.openai.com/auth/callback?code=code-plain"}`,
	}
	fake.responses["/api/accounts/oauth/token"] = passwordAuthResponse{
		status: 200,
		body:   `{"access_token":"a","refresh_token":"r","id_token":"i"}`,
	}

	tokens, err := LoginWithPassword(context.Background(), fake.factory(), "u@x.com", "pw", "")
	if err != nil {
		t.Fatalf("LoginWithPassword: %v", err)
	}
	if tokens["access_token"] != "a" {
		t.Errorf("tokens = %v", tokens)
	}
	if fake.find("POST", "/api/accounts/mfa/verify") != nil {
		t.Error("mfa/verify must not be called when the callback already has a code")
	}
}

// 有 TOTP 页面但没有 totp_secret → need_totp（不能静默当成功）。
func TestLoginWithPasswordNeedsTOTP(t *testing.T) {
	fake := newPasswordAuthFake()
	fake.responses["/api/accounts/authorize"] = passwordAuthResponse{status: 200, body: "ok"}
	fake.responses["/api/accounts/password/verify"] = passwordAuthResponse{
		status: 200,
		body: `{"continue_url":"https://auth.openai.com/mfa-challenge/f1",` +
			`"page":{"type":"mfa_challenge","payload":{"factor_id":"f1"}}}`,
	}

	_, err := LoginWithPassword(context.Background(), fake.factory(), "u@x.com", "pw", "")
	if err == nil {
		t.Fatal("missing totp_secret must fail")
	}
	var loginErr *PasswordLoginError
	if !errors.As(err, &loginErr) || loginErr.Code != PasswordLoginNeedTOTP {
		t.Fatalf("error = %v, want code %s", err, PasswordLoginNeedTOTP)
	}
	if fake.find("POST", "/api/accounts/mfa/verify") != nil {
		t.Error("must not call mfa/verify without a TOTP secret")
	}
}

// mfa_challenge 但 payload 里没有 factor_id / factors → no_auth_code 且带 detail。
func TestLoginWithPasswordMFAWithoutFactorID(t *testing.T) {
	fake := newPasswordAuthFake()
	fake.responses["/api/accounts/authorize"] = passwordAuthResponse{status: 200, body: "ok"}
	fake.responses["/api/accounts/password/verify"] = passwordAuthResponse{
		status: 200,
		body: `{"continue_url":"https://auth.openai.com/mfa-challenge",` +
			`"page":{"type":"mfa_challenge","payload":{}}}`,
	}
	_, err := LoginWithPassword(context.Background(), fake.factory(), "u@x.com", "pw",
		"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ")
	var loginErr *PasswordLoginError
	if !errors.As(err, &loginErr) || loginErr.Code != PasswordLoginNoAuthCode {
		t.Fatalf("error = %v, want %s", err, PasswordLoginNoAuthCode)
	}
	if loginErr.Detail["page"] == nil {
		t.Error("detail should include the page for debugging")
	}
}

// factor_id 缺失时退回 factors[] 里第一个 totp 因子。
func TestLoginWithPasswordMFAFactorListFallback(t *testing.T) {
	fake := newPasswordAuthFake()
	fake.responses["/api/accounts/authorize"] = passwordAuthResponse{status: 200, body: "ok"}
	fake.responses["/api/accounts/password/verify"] = passwordAuthResponse{
		status: 200,
		body: `{"continue_url":"https://auth.openai.com/mfa-challenge",` +
			`"page":{"type":"mfa_challenge","payload":{"factors":[` +
			`{"id":"recovery-1","factor_type":"recovery","is_recovery":true},` +
			`{"id":"totp-9","factor_type":"totp","is_recovery":false}]}}}`,
	}
	fake.responses["/api/accounts/mfa/verify"] = passwordAuthResponse{
		status: 200,
		body:   `{"continue_url":"https://platform.openai.com/auth/callback?code=c1"}`,
	}
	fake.responses["/api/accounts/oauth/token"] = passwordAuthResponse{
		status: 200,
		body:   `{"access_token":"a","refresh_token":"r"}`,
	}
	if _, err := LoginWithPassword(context.Background(), fake.factory(), "u@x.com", "pw",
		"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"); err != nil {
		t.Fatalf("LoginWithPassword: %v", err)
	}
	mfa := fake.find("POST", "/api/accounts/mfa/verify")
	var body map[string]any
	_ = json.Unmarshal(mfa.body, &body)
	if body["id"] != "totp-9" {
		t.Errorf("mfa id = %v, want totp-9 (skip the recovery factor)", body["id"])
	}
}

// mfa/verify 失败要带状态码与上游错误码。
func TestLoginWithPasswordMFAVerifyFailure(t *testing.T) {
	fake := newPasswordAuthFake()
	fake.responses["/api/accounts/authorize"] = passwordAuthResponse{status: 200, body: "ok"}
	fake.responses["/api/accounts/password/verify"] = passwordAuthResponse{
		status: 200,
		body: `{"continue_url":"https://auth.openai.com/mfa-challenge/f1",` +
			`"page":{"type":"mfa_challenge","payload":{"factor_id":"f1"}}}`,
	}
	fake.responses["/api/accounts/mfa/verify"] = passwordAuthResponse{
		status: 401,
		body:   `{"error":{"code":"invalid_otp","message":"Incorrect code"}}`,
	}
	_, err := LoginWithPassword(context.Background(), fake.factory(), "u@x.com", "pw",
		"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ")
	var loginErr *PasswordLoginError
	if !errors.As(err, &loginErr) || loginErr.Code != "mfa_verify_failed_401" {
		t.Fatalf("error = %v, want mfa_verify_failed_401", err)
	}
	if !strings.Contains(loginErr.Error(), "Incorrect code") {
		t.Errorf("message should surface upstream detail: %v", loginErr)
	}
}

// 邮箱验证码分支 → need_verification_code（Python 也是这个码）。
func TestLoginWithPasswordEmailOTP(t *testing.T) {
	fake := newPasswordAuthFake()
	fake.responses["/api/accounts/authorize"] = passwordAuthResponse{status: 200, body: "ok"}
	fake.responses["/api/accounts/password/verify"] = passwordAuthResponse{
		status: 200,
		body:   `{"page":{"type":"email_otp_verification"}}`,
	}
	_, err := LoginWithPassword(context.Background(), fake.factory(), "u@x.com", "pw", "")
	var loginErr *PasswordLoginError
	if !errors.As(err, &loginErr) || loginErr.Code != PasswordLoginNeedVerificationCode {
		t.Fatalf("error = %v, want %s", err, PasswordLoginNeedVerificationCode)
	}
}

// page.type 为空且没有 code → no_auth_code，错误里带上 page.type 便于排查。
func TestLoginWithPasswordUnknownPage(t *testing.T) {
	fake := newPasswordAuthFake()
	fake.responses["/api/accounts/authorize"] = passwordAuthResponse{status: 200, body: "ok"}
	fake.responses["/api/accounts/password/verify"] = passwordAuthResponse{status: 200, body: `{}`}
	_, err := LoginWithPassword(context.Background(), fake.factory(), "u@x.com", "pw", "")
	var loginErr *PasswordLoginError
	if !errors.As(err, &loginErr) || loginErr.Code != PasswordLoginNoAuthCode {
		t.Fatalf("error = %v, want %s", err, PasswordLoginNoAuthCode)
	}
	if !strings.Contains(loginErr.Message, "page.type=") {
		t.Errorf("message should mention page.type: %q", loginErr.Message)
	}
}

// password/verify 的错误码映射表。
func TestLoginWithPasswordVerifyErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantInText string
	}{
		{
			name:       "wrong password",
			status:     401,
			body:       `{"error":{"code":"","message":"Invalid credentials"}}`,
			wantCode:   PasswordLoginInvalidPassword,
			wantInText: "Invalid credentials",
		},
		{
			name:       "wrong password lowercase",
			status:     400,
			body:       `{"error":{"message":"wrong password provided"}}`,
			wantCode:   PasswordLoginInvalidPassword,
			wantInText: "wrong password",
		},
		{
			name:       "rate limited",
			status:     429,
			body:       `{"error":{"code":"rate_limit_exceeded","message":"too many attempts"}}`,
			wantCode:   PasswordLoginRateLimited,
			wantInText: "限流",
		},
		{
			name:       "unsupported region",
			status:     403,
			body:       `{"error":{"code":"unsupported_country_region_territory","message":"region"}}`,
			wantCode:   PasswordLoginUnsupportedRegion,
			wantInText: "地区",
		},
		{
			name:       "invalid state",
			status:     400,
			body:       `{"error":{"code":"invalid_state","message":"state mismatch"}}`,
			wantCode:   PasswordLoginInvalidState,
			wantInText: "state",
		},
		{
			name:       "account deactivated",
			status:     403,
			body:       `{"error":{"code":"account_deactivated","message":"deactivated"}}`,
			wantCode:   PasswordLoginAccountDeactivated,
			wantInText: "停用",
		},
		{
			name:       "unknown failure keeps status",
			status:     500,
			body:       `{"error":{"code":"server_error","message":"boom"}}`,
			wantCode:   "password_verify_failed_500",
			wantInText: "boom",
		},
		{
			name:       "non json failure",
			status:     502,
			body:       `bad gateway`,
			wantCode:   "password_verify_failed_502",
			wantInText: "bad gateway",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			fake := newPasswordAuthFake()
			fake.responses["/api/accounts/authorize"] = passwordAuthResponse{status: 200, body: "ok"}
			fake.responses["/api/accounts/password/verify"] = passwordAuthResponse{status: item.status, body: item.body}

			_, err := LoginWithPassword(context.Background(), fake.factory(), "u@x.com", "pw", "")
			var loginErr *PasswordLoginError
			if !errors.As(err, &loginErr) {
				t.Fatalf("error = %v, want *PasswordLoginError", err)
			}
			if loginErr.Code != item.wantCode {
				t.Errorf("code = %q, want %q", loginErr.Code, item.wantCode)
			}
			if !strings.Contains(loginErr.Error(), item.wantInText) {
				t.Errorf("message %q should contain %q", loginErr.Error(), item.wantInText)
			}
		})
	}
}

// authorize 重定向到 /error?payload=... 时按里面的 errorCode 报错。
func TestLoginWithPasswordAuthorizeErrorRedirect(t *testing.T) {
	fake := newPasswordAuthFake()
	payload := base64URL(`{"errorCode":"rate_limit_exceeded","message":"slow down"}`)
	fake.responses["/api/accounts/authorize"] = passwordAuthResponse{
		status:   200,
		body:     "<html>error</html>",
		finalURL: "https://auth.openai.com/error?payload=" + payload,
	}
	_, err := LoginWithPassword(context.Background(), fake.factory(), "u@x.com", "pw", "")
	var loginErr *PasswordLoginError
	if !errors.As(err, &loginErr) || loginErr.Code != PasswordLoginRateLimited {
		t.Fatalf("error = %v, want %s", err, PasswordLoginRateLimited)
	}
	if fake.find("SENTINEL", "") != nil {
		t.Error("must stop before sentinel when authorize already failed")
	}
}

// authorize 非 200/302 要报 authorize_failed_<status>。
func TestLoginWithPasswordAuthorizeFailure(t *testing.T) {
	fake := newPasswordAuthFake()
	fake.responses["/api/accounts/authorize"] = passwordAuthResponse{status: 500, body: "nope"}
	_, err := LoginWithPassword(context.Background(), fake.factory(), "u@x.com", "pw", "")
	var loginErr *PasswordLoginError
	if !errors.As(err, &loginErr) || loginErr.Code != "authorize_failed_500" {
		t.Fatalf("error = %v, want authorize_failed_500", err)
	}
}

// 换 token 失败（没有 refresh_token）→ token_exchange_failed。
func TestLoginWithPasswordTokenExchangeFailure(t *testing.T) {
	fake := newPasswordAuthFake()
	fake.responses["/api/accounts/authorize"] = passwordAuthResponse{status: 200, body: "ok"}
	fake.responses["/api/accounts/password/verify"] = passwordAuthResponse{
		status: 200,
		body:   `{"continue_url":"https://platform.openai.com/auth/callback?code=c1"}`,
	}
	fake.responses["/api/accounts/oauth/token"] = passwordAuthResponse{
		status: 200,
		body:   `{"access_token":"a"}`,
	}
	_, err := LoginWithPassword(context.Background(), fake.factory(), "u@x.com", "pw", "")
	var loginErr *PasswordLoginError
	if !errors.As(err, &loginErr) || loginErr.Code != "token_exchange_failed" {
		t.Fatalf("error = %v, want token_exchange_failed", err)
	}
	if !strings.Contains(loginErr.Message, "refresh_token") {
		t.Errorf("message should explain the missing refresh_token: %q", loginErr.Message)
	}
}

// 凭据缺失在发起任何请求之前就要报明确错误码。
func TestLoginWithPasswordMissingCredentials(t *testing.T) {
	fake := newPasswordAuthFake()
	_, err := LoginWithPassword(context.Background(), fake.factory(), "  ", "pw", "")
	var loginErr *PasswordLoginError
	if !errors.As(err, &loginErr) || loginErr.Code != PasswordLoginNoEmail {
		t.Fatalf("missing email: %v", err)
	}
	_, err = LoginWithPassword(context.Background(), fake.factory(), "u@x.com", "  ", "")
	if !errors.As(err, &loginErr) || loginErr.Code != PasswordLoginNoPassword {
		t.Fatalf("missing password: %v", err)
	}
	if len(fake.requests) != 0 {
		t.Errorf("no upstream request should be made, got %d", len(fake.requests))
	}
}

// 没有注入 transport 时要报 transport_not_configured（而不是 panic）。
func TestLoginWithPasswordWithoutTransport(t *testing.T) {
	_, err := LoginWithPassword(context.Background(), nil, "u@x.com", "pw", "")
	var loginErr *PasswordLoginError
	if !errors.As(err, &loginErr) || loginErr.Code != "transport_not_configured" {
		t.Fatalf("error = %v", err)
	}
}

// 构建 transport 失败要原样带出来。
func TestLoginWithPasswordTransportBuildFailure(t *testing.T) {
	factory := func(proxyURL string) (PasswordLoginTransport, error) {
		return nil, errors.New("tls profile missing")
	}
	_, err := LoginWithPassword(context.Background(), factory, "u@x.com", "pw", "")
	if err == nil || !strings.Contains(err.Error(), "tls profile missing") {
		t.Fatalf("error = %v", err)
	}
}

// jsonKeyOrder 按出现顺序返回顶层 JSON 对象的键。
func jsonKeyOrder(t *testing.T, payload []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("payload is not a JSON object: %s", payload)
	}
	order := make([]string, 0, 4)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		key, _ := token.(string)
		order = append(order, key)
		var discard any
		if err := decoder.Decode(&discard); err != nil {
			t.Fatalf("skip value: %v", err)
		}
	}
	return order
}

// base64URL 生成不带 padding 的 base64（authorize 错误页的 payload 形态）。
func base64URL(text string) string {
	return strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(text)), "=")
}
