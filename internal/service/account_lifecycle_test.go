package service

import (
	"context"
	"testing"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/upstream"
)

// 回归点：MFA 步骤必须识别 account_deactivated。
//
// 实测（2026-10-27）：被封账号的密码校验能通过，上游在 mfa/verify
// 才回 403 + code=account_deactivated。原实现只在这里做通用兜底
// （mfa_verify_failed_403），导致封号被判成「非确定性失败」——
// 不禁用、继续按 6 小时退避重试，永远出不来。
func TestMfaVerifyRecognizesAccountDeactivated(t *testing.T) {
	body := []byte(`{"error":{"code":"account_deactivated","message":"You do not have an account because it has been deleted or deactivated."}}`)
	data := map[string]any{
		"error": map[string]any{
			"code":    "account_deactivated",
			"message": "You do not have an account because it has been deleted or deactivated.",
		},
	}
	err := mfaVerifyError(403, data, body)
	loginErr, ok := err.(*PasswordLoginError)
	if !ok {
		t.Fatalf("err type = %T, want *PasswordLoginError", err)
	}
	if loginErr.Code != PasswordLoginAccountDeactivated {
		t.Errorf("code = %q, want %q", loginErr.Code, PasswordLoginAccountDeactivated)
	}
}

// 上游若只给 message 不给 code，也必须能认出来。
func TestMfaVerifyRecognizesDeactivatedByMessage(t *testing.T) {
	body := []byte(`{}`)
	data := map[string]any{
		"error": map[string]any{
			"message": "You do not have an account because it has been deleted or deactivated.",
		},
	}
	err := mfaVerifyError(403, data, body)
	loginErr, ok := err.(*PasswordLoginError)
	if !ok || loginErr.Code != PasswordLoginAccountDeactivated {
		t.Fatalf("message-only deactivated not recognized: %v", err)
	}
}

// 普通的 MFA 失败不能被误判成封号。
func TestMfaVerifyKeepsGenericFailure(t *testing.T) {
	data := map[string]any{"error": map[string]any{"code": "invalid_code", "message": "wrong code"}}
	err := mfaVerifyError(403, data, []byte(`{}`))
	loginErr := err.(*PasswordLoginError)
	if loginErr.Code == PasswordLoginAccountDeactivated {
		t.Error("普通 MFA 失败被误判成 account_deactivated")
	}
}

// 自动路径下确定性失败（密码错）必须禁用账号。
func TestAutoReloginDisablesOnFatalFailure(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "a@example.test", "password": "pw",
			"type": "free", "status": StatusAbnormal, "quota": 0},
	})
	if _, err := cfg.Update(map[string]any{"auto_relogin_after_refresh": true}); err != nil {
		t.Fatal(err)
	}
	svc.SetAutoRelogin(func(ctx context.Context, proxyURL, email, password, totp string) (map[string]string, error) {
		return nil, &PasswordLoginError{Code: PasswordLoginInvalidPassword, Message: "密码错误"}
	})
	verify := func(ctx context.Context, token, event string) (Account, error) {
		return nil, upstream.NewStatusError(401, nil, "/backend-api/me failed: status=401")
	}
	svc.RefreshAccounts(context.Background(), []string{"at-1"}, "", verify)

	a := svc.GetAccount("at-1")
	if a == nil {
		t.Fatal("账号不该被删除")
	}
	if got := stringValue(a["status"]); got != StatusDisabled {
		t.Errorf("status = %q, want %q（自动路径下密码错应禁用）", got, StatusDisabled)
	}
	if got := stringValue(a["disabled_reason"]); got == "" {
		t.Error("disabled_reason 未记录")
	}
}

// 自动路径下缺凭据：直接禁用（不可恢复）。
//
// 策略变更（2026-10-27）：账号无论如何无法恢复就转「禁用」。
// 早期实现是打 needs_manual 标记保持异常，实测后果是账号永远停在
// 异常、每轮白跑、用户也看不出该做什么。
func TestAutoReloginDisablesWithoutPassword(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "a@example.test",
			"type": "free", "status": StatusAbnormal, "quota": 0},
	})
	if _, err := cfg.Update(map[string]any{"auto_relogin_after_refresh": true}); err != nil {
		t.Fatal(err)
	}
	called := 0
	svc.SetAutoRelogin(func(ctx context.Context, proxyURL, email, password, totp string) (map[string]string, error) {
		called++
		return nil, nil
	})
	verify := func(ctx context.Context, token, event string) (Account, error) {
		return nil, upstream.NewStatusError(401, nil, "/backend-api/me failed: status=401")
	}
	svc.RefreshAccounts(context.Background(), []string{"at-1"}, "", verify)

	if called != 0 {
		t.Errorf("缺密码时不该调用登录执行体，实际调用 %d 次", called)
	}
	a := svc.GetAccount("at-1")
	if got := stringValue(a["status"]); got != StatusDisabled {
		t.Errorf("status = %q, want %q（缺凭据不可恢复→禁用）", got, StatusDisabled)
	}
	if got := stringValue(a["disabled_reason"]); got != PasswordLoginNoPassword {
		t.Errorf("disabled_reason = %q, want %q", got, PasswordLoginNoPassword)
	}
}

// 手动路径（有 progressID）失败时绝不能禁用。
func TestManualReloginNeverDisables(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "a@example.test", "password": "pw",
			"type": "free", "status": StatusAbnormal, "quota": 0},
	})
	login := func(ctx context.Context, proxyURL, email, password, totp string) (map[string]string, error) {
		return nil, &PasswordLoginError{Code: PasswordLoginInvalidPassword, Message: "密码错误"}
	}
	svc.InitReloginProgress("pid-1", 1)
	svc.ReloginAccounts(context.Background(), []string{"at-1"}, "pid-1", login)

	a := svc.GetAccount("at-1")
	if got := stringValue(a["status"]); got == StatusDisabled {
		t.Error("手动重登失败不该禁用账号")
	}
}

// 禁用号必须被排除出所有自动维护列表。
func TestDisabledExcludedFromAutoMaintenance(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-dis", "refresh_token": "rt-1", "email": "d@example.test",
			"type": "free", "status": StatusDisabled, "quota": 0},
		{"access_token": "at-ok", "refresh_token": "rt-2", "email": "o@example.test",
			"type": "free", "status": StatusNormal, "quota": 10},
	})

	contains := func(list []string, want string) bool {
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	}

	for name, list := range map[string][]string{
		"ListRefreshTokenKeepaliveTokens": svc.ListRefreshTokenKeepaliveTokens(),
		"ListExpiringAccessTokens":        svc.ListExpiringAccessTokens(),
		"ListMaintainableTokens":          svc.ListMaintainableTokens(),
	} {
		if contains(list, "at-dis") {
			t.Errorf("%s 仍包含禁用账号", name)
		}
		// keepalive / expiring 有额外的时间条件（3 天锚点、24h 过期窗口），
		// 新建的账号本来就不该入选 —— 这里只断言禁用号被排除。
		_ = contains(list, "at-ok")
	}
}

// 刷新成功不能把「禁用」复活成「正常」。
func TestRefreshDoesNotReviveDisabled(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-dis", "type": "free", "status": StatusDisabled, "quota": 0},
	})
	ok := svc.applyRefreshedAccount("at-dis", "at-dis", Account{
		"status": StatusNormal, "quota": 42, "email": "d@example.test",
	})
	if !ok {
		t.Fatal("applyRefreshedAccount 返回 false")
	}
	a := svc.GetAccount("at-dis")
	if got := stringValue(a["status"]); got != StatusDisabled {
		t.Errorf("status = %q, 禁用被刷新复活了", got)
	}
	if got := intValue(a["quota"]); got != 0 {
		t.Errorf("quota = %d, 禁用号额度不该被刷新覆盖", got)
	}
}

// 禁用号被 401 命中时不能被降级成异常。
func TestInvalidTokenDoesNotDowngradeDisabled(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-dis", "type": "free", "status": StatusDisabled, "quota": 0,
			"invalid_count":   3,
			"created_at":      time.Now().Local().Add(-time.Hour).Format("2006-01-02 15:04:05"),
			"last_invalid_at": time.Now().Local().Add(-10 * time.Minute).Format("2006-01-02 15:04:05")},
	})
	svc.RecordInvalidTokenSeen("at-dis", "refresh", "401", true)
	a := svc.GetAccount("at-dis")
	if got := stringValue(a["status"]); got != StatusDisabled {
		t.Errorf("status = %q, 禁用被降级成异常了", got)
	}
}

// 首次失效：标异常 + quota 归零，但绝不删除。
func TestFirstInvalidMarksAbnormalWithoutRemoval(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "type": "free", "status": StatusNormal, "quota": 25,
			"created_at": time.Now().Local().Add(-time.Hour).Format("2006-01-02 15:04:05")},
	})
	if _, err := cfg.Update(map[string]any{"auto_remove_invalid_accounts": true}); err != nil {
		t.Fatal(err)
	}
	if svc.RecordInvalidTokenSeen("at-1", "refresh", "401", true) {
		t.Error("首次失效不应允许删除")
	}
	a := svc.GetAccount("at-1")
	if a == nil {
		t.Fatal("账号不该被删除")
	}
	if got := stringValue(a["status"]); got != StatusAbnormal {
		t.Errorf("status = %q, want %q", got, StatusAbnormal)
	}
	if got := intValue(a["quota"]); got != 0 {
		t.Errorf("quota = %d, want 0", got)
	}
}
