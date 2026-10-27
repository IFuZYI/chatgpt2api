package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/api"
	"github.com/IFuZYI/chatgpt2api/internal/service"
	"github.com/IFuZYI/chatgpt2api/internal/upstream"
)

// AUDIT-5: 「刷新选中」不能绕过禁用隔离。
//
// 场景：用户选中一个已禁用的账号点「刷新选中账号信息」。
// 现实现只在不传 access_tokens 时才过滤禁用号；显式传 token 时原样放行，
// 而刷新成功会合并上游 status（正常/限流）→ 禁用被复活。
//
// 这与产品要求「禁用后不再自动维护、刷新」冲突：禁用号的解禁通道
// 只能是「尝试恢复」（明确的人工动作），不该被一个刷新动作静默解开。
func TestRefreshExplicitTokensSkipsDisabledAccounts(t *testing.T) {
	deps := newTestDeps(t)
	// 注入一个恒成功的账号信息拉取器，模拟"上游认为这个号正常"。
	deps.AccountInfoFetcher = func(ctx context.Context, accessToken string) (service.Account, error) {
		return service.Account{"status": "正常", "quota": 42, "email": "x@example.test"}, nil
	}
	deps.Accts.AddAccountItems([]map[string]any{
		{"access_token": "at-disabled", "type": "free", "status": "禁用", "quota": 0,
			"disabled_reason": "account_deactivated"},
		{"access_token": "at-normal", "type": "free", "status": "正常", "quota": 10},
	}, nil)
	router := api.NewRouter(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/accounts/refresh",
		bytes.NewBufferString(`{"access_tokens":["at-disabled","at-normal"]}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("刷新请求失败: %d %s", w.Code, w.Body.String())
	}
	var started map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &started)
	progressID, _ := started["progress_id"].(string)

	// 等进度结束
	for i := 0; i < 60; i++ {
		req = httptest.NewRequest(http.MethodGet, "/api/accounts/refresh/progress/"+progressID, nil)
		req.Header.Set("Authorization", "Bearer test")
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var payload map[string]any
		if json.Unmarshal(w.Body.Bytes(), &payload) == nil {
			if done, _ := payload["done"].(bool); done {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	a := deps.Accts.GetAccount("at-disabled")
	if a == nil {
		t.Fatal("禁用账号不该被删除")
	}
	if got := a["status"]; got != "禁用" {
		t.Errorf("禁用账号被刷新复活成 %v —— 刷新不该绕过禁用隔离", got)
	}
	// 正常号仍必须被刷新（额度写回）。
	n := deps.Accts.GetAccount("at-normal")
	if n == nil {
		t.Fatal("正常账号丢失")
	}
	if n["status"] != "正常" {
		t.Errorf("正常账号状态被改动: %v", n["status"])
	}
}

// AUDIT-6: 自动重登不能复活「禁用」账号。
//
// 场景：用户选中一个禁用号点「刷新」。刷新探测到 401 → 触发自动重登
// （progressID="" → automatic=true）→ 密码仍然可用 → 重登成功 →
// ApplyReloginTokens 把 status 写成「正常」→ 禁用被静默解开。
//
// applyRefreshedAccount 的禁用保护挡不住这条路径：它保护的是"刷新成功
// 合并状态"，而这里是"重登成功改写状态"。产品要求是「禁用后不再自动
// 维护、刷新」——解禁通道只能是明确的「尝试恢复」。
func TestAutoReloginDoesNotReviveDisabled(t *testing.T) {
	deps := newTestDeps(t)
	calls := 0
	deps.PasswordLoginClient = func(proxyURL string) (service.PasswordLoginTransport, error) {
		calls++
		return &passwordLoginStub{}, nil
	}
	deps.Accts.SetAutoRelogin(deps.PasswordLoginFunc())
	// 刷新探测返回 401 → 进入 invalidTokens → 触发自动重登
	deps.AccountInfoFetcher = func(ctx context.Context, accessToken string) (service.Account, error) {
		return nil, upstream.NewStatusError(401, nil, "/backend-api/me failed: status=401")
	}
	deps.Accts.AddAccountItems([]map[string]any{
		{"access_token": "at-disabled", "email": "d@example.test", "password": "pw",
			"type": "free", "status": "禁用", "quota": 0,
			"disabled_reason": "account_deactivated"},
	}, nil)
	if _, err := deps.Config.Update(map[string]any{"auto_relogin_after_refresh": true}); err != nil {
		t.Fatal(err)
	}
	router := api.NewRouter(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/accounts/refresh",
		bytes.NewBufferString(`{"access_tokens":["at-disabled"]}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var started map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &started)
	progressID, _ := started["progress_id"].(string)
	for i := 0; i < 60; i++ {
		req = httptest.NewRequest(http.MethodGet, "/api/accounts/refresh/progress/"+progressID, nil)
		req.Header.Set("Authorization", "Bearer test")
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var payload map[string]any
		if json.Unmarshal(w.Body.Bytes(), &payload) == nil {
			if done, _ := payload["done"].(bool); done {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if calls != 0 {
		t.Errorf("禁用号不该被自动重登（登录执行体被调用 %d 次）", calls)
	}
	a := deps.Accts.GetAccount("at-disabled")
	if a == nil {
		t.Fatal("禁用账号不该被删除")
	}
	if got := a["status"]; got != "禁用" {
		t.Errorf("status = %v, 禁用被自动重登复活了", got)
	}
}
