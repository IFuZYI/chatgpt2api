package service

import (
	"testing"
)

// 使用统计（last_used_at / quota / success / fail）变化不应自增版本号。
//
// 回归点：saveAccountsLocked 在每个请求都会跑（MarkTextUsed 更新 last_used_at、
// MarkImageResult 更新 quota）。若无条件自增版本号，模型缓存永远失效，
// /v1/models 每次都要重新拉上游（实测 63ms → 1348ms 的退化）。
func TestVersionStableOnUsageStatsChange(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-1", "type": "free", "status": StatusNormal, "quota": 25},
	})
	before := svc.Version()

	// 模拟一次图片记账：quota / success 变化，但结构性内容不变
	svc.MarkImageResult("tok-1", true)
	svc.MarkTextUsed("tok-1")

	if after := svc.Version(); after != before {
		t.Errorf("version bumped on usage stats change: %d -> %d (model cache would be invalidated every request)",
			before, after)
	}
}

// 账号集合变化必须自增版本号（模型目录需要跟着变）。
func TestVersionBumpsOnStructuralChange(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-1", "type": "free", "status": StatusNormal, "quota": 25},
	})
	before := svc.Version()

	svc.AddAccountItems(nil, []string{"tok-2"})

	if after := svc.Version(); after <= before {
		t.Errorf("version did not bump when account added: %d -> %d (model cache would serve stale data)",
			before, after)
	}
}

// 账号类型变化必须自增（free → Plus 会改变可用模型集）。
func TestVersionBumpsOnTypeChange(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-1", "type": "free", "status": StatusNormal, "quota": 25},
	})
	before := svc.Version()

	svc.UpdateAccount("tok-1", Account{"type": "Plus"}, false)

	if after := svc.Version(); after <= before {
		t.Errorf("version did not bump on type change: %d -> %d", before, after)
	}
}

// 账号状态变化必须自增（正常 → 禁用会让该账号的模型不再投放）。
func TestVersionBumpsOnStatusChange(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-1", "type": "free", "status": StatusNormal, "quota": 25},
	})
	before := svc.Version()

	svc.UpdateAccount("tok-1", Account{"status": StatusDisabled}, false)

	if after := svc.Version(); after <= before {
		t.Errorf("version did not bump on status change: %d -> %d", before, after)
	}
}
