package service

import (
	"errors"
	"testing"
)

// newEditableAccountService 建一个只装指定账号的号池。
func newEditableAccountService(t *testing.T, items []map[string]any) *AccountService {
	t.Helper()
	svc, _ := newTestAccountService(t, nil)
	if len(items) > 0 {
		svc.AddAccountItems(items, nil)
	}
	return svc
}

// 只有 Plus/Team/Pro/Enterprise 能跑可编辑文件；free 与禁用/异常号必须被跳过。
func TestEditableFileAccessTokenFiltersByPlanType(t *testing.T) {
	svc := newEditableAccountService(t, []map[string]any{
		{"access_token": "tok-free", "type": "free", "status": StatusNormal},
		{"access_token": "tok-disabled", "type": "plus", "status": StatusDisabled},
		{"access_token": "tok-abnormal", "type": "plus", "status": StatusAbnormal},
		{"access_token": "tok-plus", "type": "plus", "status": StatusNormal},
		{"access_token": "tok-team", "type": "team", "status": StatusNormal},
	})

	token, err := svc.EditableFileAccessToken()
	if err != nil {
		t.Fatalf("EditableFileAccessToken: %v", err)
	}
	// 空 last_used_at 排最前；两者都空时按 token 字典序 → tok-plus。
	if token != "tok-plus" {
		t.Errorf("token = %q, want tok-plus", token)
	}
}

// 没有合格账号时返回 Python 的原文案。
func TestEditableFileAccessTokenWithoutCandidates(t *testing.T) {
	svc := newEditableAccountService(t, []map[string]any{
		{"access_token": "tok-free", "type": "free", "status": StatusNormal},
	})
	if _, err := svc.EditableFileAccessToken(); !errors.Is(err, ErrNoEditableAccount) {
		t.Fatalf("error = %v, want ErrNoEditableAccount", err)
	}
	if got := ErrNoEditableAccount.Error(); got != "no available plus/team/pro account" {
		t.Errorf("message = %q, want Python wording", got)
	}
}

// 排序用 last_used_at 升序：最久没用的先用。
func TestEditableFileAccessTokenPrefersLeastRecentlyUsed(t *testing.T) {
	svc := newEditableAccountService(t, []map[string]any{
		{"access_token": "tok-recent", "type": "pro", "status": StatusNormal, "last_used_at": "2026-01-02 00:00:00"},
		{"access_token": "tok-stale", "type": "pro", "status": StatusNormal, "last_used_at": "2025-01-01 00:00:00"},
	})
	token, err := svc.EditableFileAccessToken()
	if err != nil {
		t.Fatalf("EditableFileAccessToken: %v", err)
	}
	if token != "tok-stale" {
		t.Errorf("token = %q, want tok-stale (least recently used)", token)
	}
}

// 不看额度：quota=0 的 plus 号仍然可以跑（可编辑文件不消耗 image_gen 额度）。
func TestEditableFileAccessTokenIgnoresQuota(t *testing.T) {
	svc := newEditableAccountService(t, []map[string]any{
		{"access_token": "tok-zero", "type": "plus", "status": StatusNormal, "quota": 0},
	})
	token, err := svc.EditableFileAccessToken()
	if err != nil {
		t.Fatalf("EditableFileAccessToken: %v", err)
	}
	if token != "tok-zero" {
		t.Errorf("token = %q, want tok-zero", token)
	}
}

// 邮箱查询：取不到时给空串，不 panic。
func TestEditableFileAccountEmail(t *testing.T) {
	svc := newEditableAccountService(t, []map[string]any{
		{"access_token": "tok-plus", "type": "plus", "status": StatusNormal, "email": "a@b.c"},
	})
	if got := svc.EditableFileAccountEmail("tok-plus"); got != "a@b.c" {
		t.Errorf("email = %q, want a@b.c", got)
	}
	if got := svc.EditableFileAccountEmail("missing"); got != "" {
		t.Errorf("email for missing token = %q, want empty", got)
	}
}
