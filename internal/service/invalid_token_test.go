package service

import (
	"testing"

	"github.com/basketikun/chatgpt2api/internal/upstream"
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
func TestIsTokenInvalidFailureOnlyFor401And403(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{401, true},
		{403, true},
		{429, false},
		{500, false},
		{0, false},
	}
	for _, tc := range cases {
		err := upstream.NewStatusError(tc.status, nil, "test")
		if got := isTokenInvalidFailure(err); got != tc.want {
			t.Errorf("status %d → %v, want %v", tc.status, got, tc.want)
		}
	}
	if isTokenInvalidFailure(nil) {
		t.Error("nil error should not be treated as token invalid")
	}
}
