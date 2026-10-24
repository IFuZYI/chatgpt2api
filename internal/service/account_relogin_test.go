package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// reloginLogin 是测试用的登录执行体：记录调用参数，按 email 决定结果。
type reloginLogin struct {
	calls   []reloginCall
	results map[string]reloginResult
}

type reloginCall struct {
	email      string
	password   string
	totpSecret string
}

type reloginResult struct {
	tokens map[string]string
	err    error
}

func (r *reloginLogin) login(ctx context.Context, email, password, totpSecret string) (map[string]string, error) {
	r.calls = append(r.calls, reloginCall{email: email, password: password, totpSecret: totpSecret})
	result, ok := r.results[email]
	if !ok {
		return nil, fmt.Errorf("unexpected email %s", email)
	}
	return result.tokens, result.err
}

// 成功：token 三件套写回号池、source_type 变 password、状态回正常，
// 旧 token 记进别名表（前端手里的旧值仍能解析）。
func TestReloginAccountsWritesBackTokens(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{
			"access_token": "old-token",
			"email":        "user@example.com",
			"password":     "pw-1",
			"totp_secret":  "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ",
			"source_type":  "web",
			"status":       StatusAbnormal,
			"type":         "plus",
		},
	})

	login := &reloginLogin{results: map[string]reloginResult{
		"user@example.com": {tokens: map[string]string{
			"access_token":  "new-token",
			"refresh_token": "new-refresh",
			"id_token":      "new-id",
		}},
	}}

	const progressID = "relogin-ok"
	result := svc.ReloginAccounts(context.Background(), []string{"old-token"}, progressID, login.login)

	if result["relogined"] != 1 {
		t.Errorf("relogined = %v, want 1", result["relogined"])
	}
	if result["skipped"] != 0 {
		t.Errorf("skipped = %v, want 0", result["skipped"])
	}
	if errorsList, ok := result["errors"].([]any); !ok || len(errorsList) != 0 {
		t.Errorf("errors = %v, want empty", result["errors"])
	}

	// 登录执行体必须收到账号里的三个凭据（含 totp_secret）。
	if len(login.calls) != 1 {
		t.Fatalf("login calls = %d, want 1", len(login.calls))
	}
	call := login.calls[0]
	if call.email != "user@example.com" || call.password != "pw-1" ||
		call.totpSecret != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" {
		t.Errorf("login call = %+v", call)
	}

	// 新 token 成为账号主键。
	account := svc.GetAccount("new-token")
	if account == nil {
		t.Fatal("new token missing from pool")
	}
	if account["refresh_token"] != "new-refresh" {
		t.Errorf("refresh_token = %v, want new-refresh", account["refresh_token"])
	}
	if account["id_token"] != "new-id" {
		t.Errorf("id_token = %v", account["id_token"])
	}
	if account["source_type"] != SourcePassword {
		t.Errorf("source_type = %v, want %s", account["source_type"], SourcePassword)
	}
	if account["status"] != StatusNormal {
		t.Errorf("status = %v, want %s", account["status"], StatusNormal)
	}
	if account["type"] != "plus" {
		t.Errorf("type = %v, want plus (existing plan must be preserved)", account["type"])
	}
	// 旧 token 通过别名解析到新 token。
	if alias := svc.GetAccount("old-token"); alias == nil || alias["access_token"] != "new-token" {
		t.Errorf("old token alias broken: %v", alias)
	}

	progress := svc.GetReloginProgress(progressID)
	if progress["done"] != true {
		t.Errorf("done = %v, want true", progress["done"])
	}
	entries, _ := progress["results"].([]any)
	if len(entries) != 1 {
		t.Fatalf("results = %v", progress["results"])
	}
	entry := entries[0].(map[string]any)
	if entry["status"] != "成功" {
		t.Errorf("entry status = %v, want 成功", entry["status"])
	}
	if entry["error"] != "" {
		t.Errorf("entry error = %v, want empty", entry["error"])
	}
	// relogin 的进度快照按前端契约只带 results[]（不含 result），
	// 汇总数据由 ReloginAccounts 的返回值给出。
	if _, ok := progress["result"]; ok {
		t.Error("relogin snapshot must not add a result key (frontend contract)")
	}
	if entries[0].(map[string]any)["name"] != "user@example.com" {
		t.Errorf("entry name = %v, want the account email", entries[0])
	}
}

// 缺 password 的账号必须报 no_password_in_account，且**不能**调用登录执行体。
func TestReloginAccountsMissingPassword(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-a", "email": "a@example.com", "status": StatusAbnormal},
	})
	login := &reloginLogin{results: map[string]reloginResult{}}

	result := svc.ReloginAccounts(context.Background(), []string{"tok-a"}, "relogin-nopw", login.login)

	if result["skipped"] != 1 {
		t.Errorf("skipped = %v, want 1", result["skipped"])
	}
	if result["relogined"] != 0 {
		t.Errorf("relogined = %v, want 0", result["relogined"])
	}
	errorsList := result["errors"].([]any)
	if len(errorsList) != 1 {
		t.Fatalf("errors = %v", errorsList)
	}
	message := errorsList[0].(map[string]any)["error"].(string)
	if !strings.Contains(message, PasswordLoginNoPassword) {
		t.Errorf("error = %q, want code %s", message, PasswordLoginNoPassword)
	}
	if len(login.calls) != 0 {
		t.Errorf("login must not be called without a password, got %d calls", len(login.calls))
	}

	progress := svc.GetReloginProgress("relogin-nopw")
	entries := progress["results"].([]any)
	if len(entries) != 1 {
		t.Fatalf("results = %v", progress["results"])
	}
	entry := entries[0].(map[string]any)
	if entry["status"] != "跳过" {
		t.Errorf("status = %v, want 跳过", entry["status"])
	}
	if !strings.Contains(entry["error"].(string), PasswordLoginNoPassword) {
		t.Errorf("entry error = %v", entry["error"])
	}
}

// 缺 email 的账号同样要明确报 code。
func TestReloginAccountsMissingEmail(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-b", "password": "pw", "status": StatusAbnormal},
	})
	login := &reloginLogin{results: map[string]reloginResult{}}
	result := svc.ReloginAccounts(context.Background(), []string{"tok-b"}, "", login.login)

	errorsList := result["errors"].([]any)
	if len(errorsList) != 1 || !strings.Contains(errorsList[0].(map[string]any)["error"].(string), PasswordLoginNoEmail) {
		t.Errorf("errors = %v, want %s", result["errors"], PasswordLoginNoEmail)
	}
	if len(login.calls) != 0 {
		t.Errorf("login must not be called without an email")
	}
}

// 登录失败：进 errors[]、进度记「异常」、号池里的 token 保持不动。
func TestReloginAccountsLoginFailure(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-c", "email": "c@example.com", "password": "pw", "status": StatusAbnormal},
	})
	login := &reloginLogin{results: map[string]reloginResult{
		"c@example.com": {err: &PasswordLoginError{Code: PasswordLoginInvalidPassword, Message: "密码错误"}},
	}}

	result := svc.ReloginAccounts(context.Background(), []string{"tok-c"}, "relogin-fail", login.login)

	if result["relogined"] != 0 {
		t.Errorf("relogined = %v, want 0", result["relogined"])
	}
	errorsList := result["errors"].([]any)
	if len(errorsList) != 1 {
		t.Fatalf("errors = %v", errorsList)
	}
	entry := errorsList[0].(map[string]any)
	if !strings.Contains(entry["error"].(string), PasswordLoginInvalidPassword) {
		t.Errorf("error = %v, want code %s", entry["error"], PasswordLoginInvalidPassword)
	}

	// 失败不能动 token。
	account := svc.GetAccount("tok-c")
	if account == nil || account["access_token"] != "tok-c" {
		t.Errorf("account must stay unchanged on failure: %v", account)
	}

	progress := svc.GetReloginProgress("relogin-fail")
	status := progress["results"].([]any)[0].(map[string]any)["status"]
	if status != "异常" {
		t.Errorf("status = %v, want 异常", status)
	}
}

// 账号被停用：标「禁用」+ quota 0（与 Python 的 account_deactivated 分支一致）。
func TestReloginAccountsDeactivatedMarksDisabled(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-d", "email": "d@example.com", "password": "pw", "quota": 5, "status": StatusNormal},
	})
	login := &reloginLogin{results: map[string]reloginResult{
		"d@example.com": {err: &PasswordLoginError{Code: PasswordLoginAccountDeactivated, Message: "账号已停用"}},
	}}

	svc.ReloginAccounts(context.Background(), []string{"tok-d"}, "relogin-disabled", login.login)

	account := svc.GetAccount("tok-d")
	if account["status"] != StatusDisabled {
		t.Errorf("status = %v, want %s", account["status"], StatusDisabled)
	}
	if account["quota"] != 0 {
		t.Errorf("quota = %v, want 0", account["quota"])
	}
	progress := svc.GetReloginProgress("relogin-disabled")
	status := progress["results"].([]any)[0].(map[string]any)["status"]
	if status != "禁用" {
		t.Errorf("status = %v, want 禁用", status)
	}
}

// 不存在的 token 要报「账号不存在」，而不是静默跳过。
func TestReloginAccountsUnknownToken(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-e", "email": "e@example.com", "password": "pw"},
	})
	login := &reloginLogin{results: map[string]reloginResult{}}
	result := svc.ReloginAccounts(context.Background(), []string{"ghost"}, "relogin-ghost", login.login)

	errorsList := result["errors"].([]any)
	if len(errorsList) != 1 {
		t.Fatalf("errors = %v", errorsList)
	}
	if errorsList[0].(map[string]any)["error"] != "账号不存在" {
		t.Errorf("error = %v, want 账号不存在", errorsList[0])
	}
	if len(login.calls) != 0 {
		t.Error("login must not be called for an unknown token")
	}
}

// 混合批次：成功 + 缺密码 + 失败，计数与结果数组都要对得上。
func TestReloginAccountsMixedBatch(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "ok", "email": "ok@example.com", "password": "pw"},
		{"access_token": "nopw", "email": "nopw@example.com"},
		{"access_token": "bad", "email": "bad@example.com", "password": "pw"},
	})
	login := &reloginLogin{results: map[string]reloginResult{
		"ok@example.com":  {tokens: map[string]string{"access_token": "fresh", "refresh_token": "r"}},
		"bad@example.com": {err: errors.New("upstream exploded")},
	}}

	result := svc.ReloginAccounts(context.Background(), []string{"ok", "nopw", "bad", "ok"}, "relogin-mixed", login.login)

	if result["relogined"] != 1 {
		t.Errorf("relogined = %v, want 1", result["relogined"])
	}
	if result["skipped"] != 1 {
		t.Errorf("skipped = %v, want 1", result["skipped"])
	}
	// 重复 token 只处理一次 → 两条错误（nopw + bad）。
	if errorsList := result["errors"].([]any); len(errorsList) != 2 {
		t.Errorf("errors = %v, want 2 entries", errorsList)
	}
	if len(login.calls) != 2 {
		t.Errorf("login calls = %d, want 2 (ok + bad; duplicate deduped, nopw skipped)", len(login.calls))
	}
	progress := svc.GetReloginProgress("relogin-mixed")
	if progress["total"] != 3 {
		t.Errorf("total = %v, want 3 (deduped)", progress["total"])
	}
	if progress["processed"] != 3 {
		t.Errorf("processed = %v, want 3", progress["processed"])
	}
	if progress["done"] != true {
		t.Errorf("done = %v, want true", progress["done"])
	}
}

// 空 token 列表：立即完成，不留下永远 running 的进度。
func TestReloginAccountsEmpty(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)
	result := svc.ReloginAccounts(context.Background(), []string{"", "  "}, "relogin-empty", nil)
	if result["relogined"] != 0 || result["skipped"] != 0 {
		t.Errorf("result = %v", result)
	}
	progress := svc.GetReloginProgress("relogin-empty")
	if progress["done"] != true {
		t.Errorf("done = %v, want true", progress["done"])
	}
}

// 没有注入执行体时逐条报 password_login_not_configured，不能假装成功。
func TestReloginAccountsWithoutLoginFunc(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-f", "email": "f@example.com", "password": "pw"},
	})
	result := svc.ReloginAccounts(context.Background(), []string{"tok-f"}, "", nil)
	errorsList := result["errors"].([]any)
	if len(errorsList) != 1 {
		t.Fatalf("errors = %v", errorsList)
	}
	if !strings.Contains(errorsList[0].(map[string]any)["error"].(string), "password_login_not_configured") {
		t.Errorf("error = %v", errorsList[0])
	}
	if result["relogined"] != 0 {
		t.Errorf("relogined = %v, want 0", result["relogined"])
	}
}
