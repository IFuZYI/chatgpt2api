package service

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 退避抖动 ----

// 指数退避必须带抖动，否则同批失败的账号在同一时刻齐发（thundering herd）。
func TestAbnormalRetryBackoffHasJitter(t *testing.T) {
	svc := &AccountService{}
	account := Account{"status": StatusAbnormal, "invalid_count": 6}
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[svc.abnormalRetryDelay(account)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("退避没有抖动：50 次只得到 %d 个不同值", len(seen))
	}
}

func TestAbnormalRetryBackoffJitterBounded(t *testing.T) {
	svc := &AccountService{}
	account := Account{"invalid_count": 30}
	for i := 0; i < 200; i++ {
		got := svc.abnormalRetryDelay(account)
		if got > abnormalRetryMaxDelay || got < abnormalRetryMaxDelay/2 {
			t.Fatalf("退避 %v 越界（应在 [%v, %v]）", got, abnormalRetryMaxDelay/2, abnormalRetryMaxDelay)
		}
	}
}

// ---- 成功路径必须清零失败状态 ----

func TestSuccessfulRefreshResetsInvalidCount(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "type": "free", "status": StatusAbnormal, "quota": 0,
			"invalid_count": 5, "last_invalid_at": nowLocalOffsetHours(-1)},
	})
	svc.applyRefreshedAccount("at-1", "at-1", Account{"status": StatusNormal, "quota": 10})
	a := svc.GetAccount("at-1")
	if got := intValue(a["invalid_count"]); got != 0 {
		t.Errorf("invalid_count = %d, 成功后应清零", got)
	}
	if got := stringValue(a["last_invalid_at"]); got != "" {
		t.Errorf("last_invalid_at = %q, 成功后应清空", got)
	}
}

func TestSuccessfulReloginResetsFailureState(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "type": "free", "status": StatusAbnormal, "quota": 0,
			"invalid_count": 7, "last_invalid_at": nowLocalOffsetHours(-1)},
	})
	if !svc.ApplyReloginTokens("at-1", map[string]string{"access_token": "at-2", "refresh_token": "rt"}) {
		t.Fatal("轮换失败")
	}
	if got := intValue(svc.GetAccount("at-2")["invalid_count"]); got != 0 {
		t.Errorf("invalid_count = %d, 重登成功后应清零", got)
	}
}

// ---- 重登去重必须按账号身份 ----

// token 轮换会建立别名，同一账号有两个字符串；按字符串去重会放过并发双登录。
func TestReloginDedupResolvesAlias(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-old", "email": "a@example.test", "password": "pw",
			"type": "free", "status": StatusAbnormal, "quota": 0},
	})
	if !svc.ApplyReloginTokens("at-old", map[string]string{"access_token": "at-new", "refresh_token": "rt"}) {
		t.Fatal("轮换失败")
	}
	if !svc.beginRelogin("at-old") {
		t.Fatal("首次占位应成功")
	}
	if svc.beginRelogin("at-new") {
		t.Error("同一账号的两个 token 字符串都拿到重登权 → 并发双登录")
	}
	svc.endRelogin("at-old")
}

func TestReloginDedupsAliasesWithinOneCall(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "a@example.test", "password": "pw",
			"type": "free", "status": StatusAbnormal, "quota": 0},
	})
	calls := 0
	login := func(ctx context.Context, proxyURL, email, password, totp string) (map[string]string, error) {
		calls++
		return map[string]string{"access_token": "at-1", "refresh_token": "rt"}, nil
	}
	svc.InitReloginProgress("pid", 2)
	svc.ReloginAccounts(context.Background(), []string{"at-1", "at-1"}, "pid", login)
	if calls != 1 {
		t.Errorf("同一账号被登录 %d 次，应为 1", calls)
	}
}

// panic 时必须释放重登占用位（否则该账号永久无法重登）。
func TestReloginReleasesSlotOnPanic(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "a@example.test", "password": "pw",
			"type": "free", "status": StatusAbnormal, "quota": 0},
	})
	func() {
		defer func() { _ = recover() }()
		svc.ReloginAccounts(context.Background(), []string{"at-1"}, "",
			func(ctx context.Context, proxyURL, email, password, totp string) (map[string]string, error) {
				panic("upstream blew up")
			})
	}()
	if !svc.beginRelogin("at-1") {
		t.Fatal("panic 后重登占用位泄漏")
	}
	svc.endRelogin("at-1")
}

// ---- 代理绑定 ----

func TestReloginPassesAccountProxyToLogin(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "a@example.test", "password": "pw",
			"type": "free", "status": StatusAbnormal, "quota": 0,
			"proxy": "socks5h://acct:pw@10.0.0.9:1080"},
	})
	if _, err := cfg.Update(map[string]any{"proxy": "http://global:8080"}); err != nil {
		t.Fatal(err)
	}
	var got string
	svc.ReloginAccounts(context.Background(), []string{"at-1"}, "",
		func(ctx context.Context, proxyURL, email, password, totp string) (map[string]string, error) {
			got = proxyURL
			return map[string]string{"access_token": "at-2"}, nil
		})
	if got != "socks5h://acct:pw@10.0.0.9:1080" {
		t.Errorf("重登代理 = %q, 期望账号自己的代理", got)
	}
}

func TestKeepalivePassesAccountProxyToRefresh(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-1", "type": "free",
			"status": StatusNormal, "quota": 10, "proxy": "socks5h://acct:pw@10.0.0.9:1080"},
	})
	var got string
	svc.KeepaliveRefreshTokens(context.Background(), []string{"at-1"},
		func(ctx context.Context, proxyURL, refreshToken string) (map[string]string, error) {
			got = proxyURL
			return map[string]string{"access_token": "at-2", "refresh_token": "rt-2"}, nil
		})
	if got != "socks5h://acct:pw@10.0.0.9:1080" {
		t.Errorf("keepalive 代理 = %q", got)
	}
}

// ProxyFor 必须归一化：否则同一配置在 tls-client 路径与新路径上解释不同
// （socks:// 与 host:port:user:pass 简写在 tls-client 上会建不起来）。
func TestProxyForNormalizesProxyString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"socks://u:p@10.0.0.1:1080", "socks5h://u:p@10.0.0.1:1080"},
		{"socks5://u:p@10.0.0.1:1080", "socks5h://u:p@10.0.0.1:1080"},
		{"socks5h://u:p@10.0.0.1:1080", "socks5h://u:p@10.0.0.1:1080"},
		{"10.0.0.1:1080:user:pass", "http://user:pass@10.0.0.1:1080"},
		{"", ""},
	}
	for _, tc := range cases {
		svc, _ := newTestAccountService(t, []map[string]any{
			{"access_token": "at-1", "type": "free", "status": StatusNormal, "proxy": tc.in},
		})
		if got := svc.ProxyFor("at-1"); got != tc.want {
			t.Errorf("ProxyFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---- 代理传输构造 ----

func TestProxyClientSupportsSOCKS5AndHTTP(t *testing.T) {
	socks, err := newProxyHTTPClient("socks5h://u:p@10.0.0.1:1080", 5*time.Second)
	if err != nil {
		t.Fatalf("socks5h 构造失败: %v", err)
	}
	tr := socks.Transport.(*http.Transport)
	if tr.DialContext == nil {
		t.Error("socks5h 未设置 DialContext（会直连）")
	}
	if tr.Proxy != nil {
		t.Error("socks5h 不该同时设置 Proxy")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("socks5h 未开启 ForceAttemptHTTP2（设 DialContext 会关闭自动 H2）")
	}

	httpClient, err := newProxyHTTPClient("http://10.0.0.1:8080", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if httpClient.Transport.(*http.Transport).Proxy == nil {
		t.Error("http 代理必须设置 Proxy")
	}

	direct, err := newProxyHTTPClient("", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if direct.Transport != nil {
		t.Errorf("空代理应返回默认 Transport，实际 %T", direct.Transport)
	}
}

// 用完必须回收空闲连接（否则每轮 keepalive 都向代理端堆积连接）。
func TestHTTPClientClosesIdleConnections(t *testing.T) {
	var mu sync.Mutex
	var opened, closed int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch state {
		case http.StateNew:
			opened++
		case http.StateClosed, http.StateHijacked:
			closed++
		}
	}
	if err := withDirectHTTPClient(5*time.Second, func(c *http.Client) error {
		for i := 0; i < 2; i++ {
			resp, err := c.Get(srv.URL)
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		return nil
	}); err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := opened > 0 && closed >= opened
		mu.Unlock()
		if done {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Errorf("空闲连接未回收: opened=%d closed=%d", opened, closed)
}

// ---- 凭据脱敏 ----

// 代理密码不得进入错误文本（会被落盘并回显到面板，且可复制）。
func TestProxyErrorsRedactCredentials(t *testing.T) {
	_, err := newProxyHTTPClient("http://acct:S3cretPw@10.0.0.9:notaport", 5*time.Second)
	if err == nil {
		t.Skip("该地址未触发解析错误")
	}
	if strings.Contains(err.Error(), "S3cretPw") {
		t.Errorf("错误文本泄漏代理密码: %v", err)
	}
}

func TestRecordedErrorsAreRedactedAtPersistenceLayer(t *testing.T) {
	const leaky = "upstream failed: http://acct:S3cretPw@10.0.0.9:1080 unreachable"
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-1", "type": "free",
			"status": StatusNormal, "quota": 10},
	})
	svc.RecordTokenRefreshError("at-1", leaky)
	svc.recordRefreshError("at-1", leaky)
	svc.recordReloginFailure("at-1", "code", leaky)
	a := svc.GetAccount("at-1")
	for _, field := range []string{"last_token_refresh_error", "last_refresh_error", "last_relogin_error"} {
		if strings.Contains(stringValue(a[field]), "S3cretPw") {
			t.Errorf("%s 未脱敏: %q", field, stringValue(a[field]))
		}
	}
}

// ---- 错误分类 ----

// invalid_username_or_password 是实测到的第二种凭据错误码；它的 message
// 是通用文案，只按 message 匹配会漏成 password_verify_failed_401。
func TestPasswordVerifyRecognizesInvalidUsernameOrPassword(t *testing.T) {
	data := map[string]any{"error": map[string]any{
		"code":    "invalid_username_or_password",
		"message": "Login failed. Please try again or seek help at https://help.openai.com/",
	}}
	if got := passwordVerifyError(401, data, []byte(`{}`)).(*PasswordLoginError).Code; got != PasswordLoginInvalidPassword {
		t.Errorf("code = %q, want %q", got, PasswordLoginInvalidPassword)
	}
}

// 不可恢复的码一律 fatal；只有环境类留在 transient。
func TestClassifyReloginFailure(t *testing.T) {
	fatal := []string{
		PasswordLoginAccountDeactivated, PasswordLoginInvalidPassword,
		PasswordLoginNoPassword, PasswordLoginNoEmail,
		PasswordLoginNeedTOTP, PasswordLoginNeedVerificationCode,
		"brand_new_unknown_code",
	}
	for _, code := range fatal {
		if got := classifyReloginFailure(code); got != reloginFatal {
			t.Errorf("%q = %v, want reloginFatal", code, got)
		}
	}
	transient := []string{
		PasswordLoginUnsupportedRegion, PasswordLoginRateLimited,
		PasswordLoginInvalidState, PasswordLoginNoAuthCode,
	}
	for _, code := range transient {
		if got := classifyReloginFailure(code); got != reloginTransient {
			t.Errorf("%q = %v, want reloginTransient（环境问题不能判死账号）", code, got)
		}
	}
}

// 未知码需要"连续两次看到同一个码"才禁用：一轮上游故障不该打掉整池。
func TestUnknownCodeRequiresSecondConfirmation(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "a@example.test", "password": "pw",
			"type": "free", "status": StatusAbnormal, "quota": 0},
	})
	if _, err := cfg.Update(map[string]any{"auto_relogin_after_refresh": true}); err != nil {
		t.Fatal(err)
	}
	svc.SetAutoRelogin(func(ctx context.Context, proxyURL, email, password, totp string) (map[string]string, error) {
		return nil, &PasswordLoginError{Code: "brand_new_code", Message: "新错误"}
	})
	login := svc.autoReloginFunc()

	svc.ReloginAccounts(context.Background(), []string{"at-1"}, "", login)
	if got := stringValue(svc.GetAccount("at-1")["status"]); got == StatusDisabled {
		t.Fatal("未知码首次出现就禁用 —— 一次上游故障会打掉整池")
	}
	svc.ReloginAccounts(context.Background(), []string{"at-1"}, "", login)
	if got := stringValue(svc.GetAccount("at-1")["status"]); got != StatusDisabled {
		t.Errorf("status = %q, 同一未知码第二次出现应确认并禁用", got)
	}
}
