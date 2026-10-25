package service

import (
	"context"
	"testing"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/upstream"
)

// token 失效时必须记 invalid_count 并按配置处理。
//
// 回归点：Go 侧完全没有这条路径 —— invalid_count 从不增长、异常状态
// 没有写入方、auto_remove_invalid_accounts 零调用。
// Python: services/account_service.py:1052-1064（remove_invalid_token）
//   - :1269-1291（_record_invalid_token_seen）。
func TestRecordInvalidTokenIncrementsCount(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-1", "type": "free", "status": StatusNormal, "quota": 25},
	})

	svc.RecordInvalidTokenSeen("tok-1", "refresh", "invalid access token", false)

	account := svc.GetAccount("tok-1")
	if account == nil {
		t.Fatal("account disappeared")
	}
	if got := intValue(account["invalid_count"]); got != 1 {
		t.Errorf("invalid_count = %d, want 1", got)
	}
	if got := stringValue(account["last_refresh_error"]); got == "" {
		t.Error("last_refresh_error not recorded")
	}
}

// 配置关闭时：标记异常 + quota 归零，不删除。
func TestRemoveInvalidTokenMarksAbnormalWhenAutoRemoveDisabled(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-1", "type": "free", "status": StatusNormal, "quota": 25},
	})
	if _, err := cfg.Update(map[string]any{"auto_remove_invalid_accounts": false}); err != nil {
		t.Fatalf("set config: %v", err)
	}

	removed := svc.RemoveInvalidToken("tok-1", "refresh", true)

	if removed {
		t.Error("removed = true, want false when auto_remove is disabled")
	}
	account := svc.GetAccount("tok-1")
	if account == nil {
		t.Fatal("account should be kept (marked abnormal), not deleted")
	}
	if got := stringValue(account["status"]); got != StatusAbnormal {
		t.Errorf("status = %q, want %q", got, StatusAbnormal)
	}
	if got := intValue(account["quota"]); got != 0 {
		t.Errorf("quota = %d, want 0", got)
	}
}

// 配置开启时：删除账号。
func TestRemoveInvalidTokenDeletesWhenAutoRemoveEnabled(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-1", "type": "free", "status": StatusNormal, "quota": 25},
	})
	if _, err := cfg.Update(map[string]any{"auto_remove_invalid_accounts": true}); err != nil {
		t.Fatalf("set config: %v", err)
	}

	removed := svc.RemoveInvalidToken("tok-1", "refresh", true)

	if !removed {
		t.Error("removed = false, want true when auto_remove is enabled")
	}
	if account := svc.GetAccount("tok-1"); account != nil {
		t.Error("account still present, want deleted")
	}
}

// 限流（429）不能被当成 token 失效 —— 否则一轮刷新会删光号池。
//
// 403 的判定按**来源**区分（不是无条件算失效）：
//   - 来自 /backend-api/me 的 403 → 账号级鉴权失败，算失效
//   - 来自 /backend-api/conversation/init 的 403 → Cloudflare 挑战/会话层，**不算**
//
// 后者是洗号池的主因：指纹漂移或坏出口 IP 会让 init 返回 403，
// 若无条件当失效处理，整池健康账号会被标异常甚至删除。
func TestIsTokenInvalidFailureByStatusAndSource(t *testing.T) {
	cases := []struct {
		name   string
		status int
		msg    string
		want   bool
	}{
		{"401 任意来源", 401, "test", true},
		{"401 来自 me", 401, "/backend-api/me failed: status=401", true},
		{"403 来自 me", 403, "/backend-api/me failed: status=403", true},
		{"403 来自 conversation/init（Cloudflare 挑战）", 403, "/backend-api/conversation/init failed: status=403", false},
		{"403 无路径信息（保守：不算失效）", 403, "test", false},
		{"429 限流", 429, "test", false},
		{"500 上游故障", 500, "test", false},
		{"0 本地错误", 0, "test", false},
	}
	for _, tc := range cases {
		err := upstream.NewStatusError(tc.status, nil, tc.msg)
		if got := isTokenInvalidFailure(err); got != tc.want {
			t.Errorf("%s: status %d → %v, want %v", tc.name, tc.status, got, tc.want)
		}
	}
	if isTokenInvalidFailure(nil) {
		t.Error("nil error should not be treated as token invalid")
	}
}

// 开启 auto_relogin_after_refresh 时必须提示不生效（不能静默）。
func TestWarnUnsupportedAutoReloginDoesNotPanic(t *testing.T) {
	svc, cfg := newTestAccountService(t, nil)
	if _, err := cfg.Update(map[string]any{"auto_relogin_after_refresh": true}); err != nil {
		t.Fatalf("set config: %v", err)
	}
	// 只验证不 panic 且能读配置（输出走 stdout，不在这里断言）
	svc.WarnUnsupportedAutoRelogin()
}

// 回归：一轮上游抖动（全部 401）**不能**删光号池。
//
// 原实现的 shouldDeferInvalidTokenLocked 用 last_invalid_at 判断，
// 而首次失效时该字段为空、函数返回 false，于是第一次 401 就直接删号。
// auto_remove_invalid_accounts 打开时，一轮坏 tick 会清空整个池子。
func TestFirstFailureNeverDeletesAccounts(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "a1", "type": "free", "status": StatusNormal, "created_at": "2020-01-01 00:00:00"},
		{"access_token": "a2", "type": "free", "status": StatusNormal, "created_at": "2020-01-01 00:00:00"},
	})
	if _, err := cfg.Update(map[string]any{"auto_remove_invalid_accounts": true}); err != nil {
		t.Fatalf("update: %v", err)
	}
	verify := func(ctx context.Context, token, event string) (Account, error) {
		return nil, upstream.NewStatusError(401, nil, "/backend-api/me failed: status=401")
	}
	svc.RefreshAccounts(context.Background(), []string{"a1", "a2"}, "", verify)

	if got := len(svc.ListAccounts()); got != 2 {
		t.Fatalf("首次失效就删号：剩余 %d 个，应为 2", got)
	}
}

// 回归：新号（创建不足 10 分钟）首次失效也不能删。
func TestNewAccountGracePreventsDeletion(t *testing.T) {
	now := time.Now().Format(accountTimeLayout)
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "fresh", "type": "free", "status": StatusNormal, "created_at": now,
			"invalid_count": 5, "last_invalid_at": "2020-01-01 00:00:00"},
	})
	if _, err := cfg.Update(map[string]any{"auto_remove_invalid_accounts": true}); err != nil {
		t.Fatalf("update: %v", err)
	}
	// 即使 invalid_count 已经很高，新号宽限仍应拦住
	if svc.RecordInvalidTokenSeen("fresh", "test", "401", true) {
		t.Error("新号宽限未生效：会走删除路径")
	}
	if got := len(svc.ListAccounts()); got != 1 {
		t.Fatalf("新号被删了：剩余 %d 个", got)
	}
}
