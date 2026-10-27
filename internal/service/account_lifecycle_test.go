package service

import (
	"context"
	"testing"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/upstream"
)

// nowLocalOffsetHours/Minutes 让「多久以前」可读且集中。
func nowLocalOffsetHours(h int) string {
	return time.Now().Local().Add(time.Duration(h) * time.Hour).Format(accountTimeLayout)
}

func nowLocalOffsetMinutes(m int) string {
	return time.Now().Local().Add(time.Duration(m) * time.Minute).Format(accountTimeLayout)
}

func status401() error {
	return upstream.NewStatusError(401, nil, "/backend-api/me failed: status=401")
}

// MFA 步骤必须识别 account_deactivated。
//
// 实测（2026-10-27）：被封账号的密码校验能通过，上游在 mfa/verify 才回
// 403 + code=account_deactivated。原实现只做通用兜底（mfa_verify_failed_403），
// 封号被降级成「非确定性失败」→ 不禁用、按 6 小时退避无限重试。
func TestMfaVerifyRecognizesAccountDeactivated(t *testing.T) {
	data := map[string]any{"error": map[string]any{
		"code":    "account_deactivated",
		"message": "You do not have an account because it has been deleted or deactivated.",
	}}
	err := mfaVerifyError(403, data, []byte(`{}`))
	if got := err.(*PasswordLoginError).Code; got != PasswordLoginAccountDeactivated {
		t.Errorf("code = %q, want %q", got, PasswordLoginAccountDeactivated)
	}
}

// 上游只给 message 不给 code 时也要能认出来。
func TestMfaVerifyRecognizesDeactivatedByMessage(t *testing.T) {
	data := map[string]any{"error": map[string]any{
		"message": "You do not have an account because it has been deleted or deactivated.",
	}}
	err := mfaVerifyError(403, data, []byte(`{}`))
	if got := err.(*PasswordLoginError).Code; got != PasswordLoginAccountDeactivated {
		t.Errorf("message-only deactivated 未识别: %q", got)
	}
}

// 普通 MFA 失败不能被误判成封号。
func TestMfaVerifyKeepsGenericFailure(t *testing.T) {
	data := map[string]any{"error": map[string]any{"code": "invalid_code", "message": "wrong code"}}
	if err := mfaVerifyError(403, data, []byte(`{}`)); err.(*PasswordLoginError).Code == PasswordLoginAccountDeactivated {
		t.Error("普通 MFA 失败被误判成 account_deactivated")
	}
}

// 自动路径下不可恢复 → 禁用；手动路径永不禁用。
func TestAutoReloginDisablesUnrecoverable(t *testing.T) {
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
	svc.ReloginAccounts(context.Background(), []string{"at-1"}, "", svc.autoReloginFunc())

	a := svc.GetAccount("at-1")
	if got := stringValue(a["status"]); got != StatusDisabled {
		t.Errorf("status = %q, want %q", got, StatusDisabled)
	}
	if got := stringValue(a["disabled_reason"]); got == "" {
		t.Error("disabled_reason 未记录")
	}
}

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
	if got := stringValue(svc.GetAccount("at-1")["status"]); got == StatusDisabled {
		t.Error("手动重登失败不该禁用账号")
	}
}

// 手动「尝试恢复」必须能解开禁用（这是禁用的唯一出口）。
func TestManualReloginRecoversDisabledAccount(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "a@example.test", "password": "pw",
			"type": "free", "status": StatusDisabled, "quota": 0,
			"disabled_reason": "need_totp", "disabled_at": "2026-10-27 12:00:00"},
	})
	login := func(ctx context.Context, proxyURL, email, password, totp string) (map[string]string, error) {
		return map[string]string{"access_token": "at-2", "refresh_token": "rt-2", "id_token": "id-2"}, nil
	}
	svc.InitReloginProgress("pid-1", 1)
	if n := intValue(svc.ReloginAccounts(context.Background(), []string{"at-1"}, "pid-1", login)["relogined"]); n != 1 {
		t.Fatalf("relogined = %d", n)
	}
	a := svc.GetAccount("at-2")
	if got := stringValue(a["status"]); got != StatusNormal {
		t.Errorf("status = %q, 手动恢复后应为正常", got)
	}
	if stringValue(a["disabled_reason"]) != "" {
		t.Error("disabled_reason 未清空")
	}
}

// 禁用号必须被排除出所有自动维护列表。
func TestDisabledExcludedFromAutoMaintenance(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-dis", "refresh_token": "rt-1", "email": "d@example.test",
			"type": "free", "status": StatusDisabled, "quota": 0},
	})
	for name, list := range map[string][]string{
		"ListRefreshTokenKeepaliveTokens": svc.ListRefreshTokenKeepaliveTokens(),
		"ListExpiringAccessTokens":        svc.ListExpiringAccessTokens(),
		"ListMaintainableTokens":          svc.ListMaintainableTokens(),
	} {
		for _, tok := range list {
			if tok == "at-dis" {
				t.Errorf("%s 仍包含禁用账号", name)
			}
		}
	}
}

// 刷新成功不能把「禁用」复活；401 也不能把它降级。
func TestRefreshNeverRevivesDisabled(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-dis", "type": "free", "status": StatusDisabled, "quota": 0},
	})
	svc.applyRefreshedAccount("at-dis", "at-dis", Account{"status": StatusNormal, "quota": 42})
	a := svc.GetAccount("at-dis")
	if got := stringValue(a["status"]); got != StatusDisabled {
		t.Errorf("status = %q, 禁用被刷新复活", got)
	}
	if got := intValue(a["quota"]); got != 0 {
		t.Errorf("quota = %d, 禁用号额度不该被覆盖", got)
	}
}

func TestInvalidTokenDoesNotDowngradeDisabled(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-dis", "type": "free", "status": StatusDisabled, "quota": 0,
			"invalid_count": 3, "created_at": nowLocalOffsetHours(-2),
			"last_invalid_at": nowLocalOffsetMinutes(-30)},
	})
	svc.RecordInvalidTokenSeen("at-dis", "refresh", "401", true)
	if got := stringValue(svc.GetAccount("at-dis")["status"]); got != StatusDisabled {
		t.Errorf("status = %q, 禁用被降级", got)
	}
}

// C1（审计发现，Critical）：批量刷新不得降级/删除禁用号。
//
// RecordInvalidTokenSeen 的保护挡不住它返回的 Remove 标志驱动的
// RemoveInvalidToken —— 那条路径原本完全没有禁用判断。触发路径是日常
// 操作：用户点「一键刷新所有账号」，前端会把含禁用行的全池 token 发上来。
func TestBulkRefreshDoesNotDowngradeDisabled(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-dis", "type": "free", "status": StatusDisabled, "quota": 0,
			"invalid_count": 3, "disabled_reason": "account_deactivated",
			"created_at": nowLocalOffsetHours(-2), "last_invalid_at": nowLocalOffsetMinutes(-30)},
	})
	if _, err := cfg.Update(map[string]any{"auto_remove_invalid_accounts": false}); err != nil {
		t.Fatal(err)
	}
	verify := func(ctx context.Context, token, event string) (Account, error) { return nil, status401() }
	svc.RefreshAccounts(context.Background(), []string{"at-dis"}, "", verify)
	a := svc.GetAccount("at-dis")
	if a == nil {
		t.Fatal("禁用账号被删除")
	}
	if got := stringValue(a["status"]); got != StatusDisabled {
		t.Errorf("status = %q, 禁用被降级", got)
	}
}

func TestBulkRefreshDoesNotDeleteDisabled(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-dis", "type": "free", "status": StatusDisabled, "quota": 0,
			"invalid_count": 5, "disabled_reason": "account_deactivated",
			"created_at": nowLocalOffsetHours(-2), "last_invalid_at": nowLocalOffsetMinutes(-30)},
	})
	if _, err := cfg.Update(map[string]any{"auto_remove_invalid_accounts": true}); err != nil {
		t.Fatal(err)
	}
	verify := func(ctx context.Context, token, event string) (Account, error) { return nil, status401() }
	svc.RefreshAccounts(context.Background(), []string{"at-dis"}, "", verify)
	if svc.GetAccount("at-dis") == nil {
		t.Fatal("禁用账号被批量刷新删除 —— 号池数据永久丢失")
	}
}

// 保护不能扩大到误伤：普通异常号仍可被删除。
func TestBulkRefreshStillRemovesAbnormalWhenEnabled(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-abn", "type": "free", "status": StatusAbnormal, "quota": 0,
			"invalid_count": 5, "created_at": nowLocalOffsetHours(-2),
			"last_invalid_at": nowLocalOffsetMinutes(-30)},
	})
	if _, err := cfg.Update(map[string]any{"auto_remove_invalid_accounts": true}); err != nil {
		t.Fatal(err)
	}
	verify := func(ctx context.Context, token, event string) (Account, error) { return nil, status401() }
	svc.RefreshAccounts(context.Background(), []string{"at-abn"}, "", verify)
	if svc.GetAccount("at-abn") != nil {
		t.Error("普通异常号应被删除（保护范围不该扩大）")
	}
}

// 重新导入不得静默复活禁用号（导入逐 key 合并，含 status）。
func TestReimportDoesNotReviveDisabledAccount(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "a@example.test", "type": "free",
			"status": StatusDisabled, "quota": 0, "disabled_reason": "account_deactivated"},
	})
	svc.AddAccountItems([]map[string]any{
		{"access_token": "at-1", "email": "a@example.test", "type": "free",
			"status": StatusNormal, "quota": 50},
	}, nil)
	a := svc.GetAccount("at-1")
	if got := stringValue(a["status"]); got != StatusDisabled {
		t.Errorf("status = %q, 重新导入把禁用号复活了", got)
	}
	if stringValue(a["disabled_reason"]) != "account_deactivated" {
		t.Error("禁用原因被导入清掉")
	}
}

// 新账号的初始状态仍应尊重导入值。
func TestImportUsesStatusForNewAccount(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)
	svc.AddAccountItems([]map[string]any{
		{"access_token": "at-new", "email": "n@example.test", "type": "free",
			"status": StatusDisabled, "quota": 0},
	}, nil)
	if got := stringValue(svc.GetAccount("at-new")["status"]); got != StatusDisabled {
		t.Errorf("status = %q, 新账号应尊重导入状态", got)
	}
}

// 未知状态不能被当作可用账号调度（黑名单写法会让导入的英文状态放行）。
func TestUnknownStatusIsNotSchedulable(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-banned", "type": "free", "status": "banned", "quota": 99},
	})
	a := svc.GetAccount("at-banned")
	if svc.isImageAccountAvailable(a) {
		t.Error("未知状态被图片链路当作可用")
	}
	if tok, err := svc.GetTextAccessToken(nil, nil, false); err == nil && tok != "" {
		t.Errorf("未知状态被文本链路选中: %q", tok)
	}
}

func TestNormalStatusStillSchedulable(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-ok", "type": "free", "status": StatusNormal, "quota": 99},
	})
	if !svc.isImageAccountAvailable(svc.GetAccount("at-ok")) {
		t.Error("正常号被误挡")
	}
}

// 在途图片任务不得覆写「异常」「禁用」。
func TestMarkImageResultDoesNotOverwriteTerminalStates(t *testing.T) {
	for _, st := range []string{StatusDisabled, StatusAbnormal} {
		svc, _ := newTestAccountService(t, []map[string]any{
			{"access_token": "at-1", "type": "free", "status": st, "quota": 1},
		})
		svc.MarkImageResult("at-1", true)
		if got := stringValue(svc.GetAccount("at-1")["status"]); got != st {
			t.Errorf("status = %q, %s 被图片任务覆写", got, st)
		}
	}
}

func TestMarkImageResultStillLimitsHealthyAccount(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "type": "free", "status": StatusNormal, "quota": 1},
	})
	svc.MarkImageResult("at-1", true)
	if got := stringValue(svc.GetAccount("at-1")["status"]); got != StatusLimited {
		t.Errorf("status = %q, 正常号额度归零应转限流", got)
	}
}
