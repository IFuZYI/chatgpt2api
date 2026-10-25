package service

import (
	"fmt"
	"testing"
	"time"
)

// 异常账号必须能最终恢复（不能永久卡住），且重试频率要收敛。
func TestAbnormalRetryBackoffRecoversEventually(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)
	// 必须用本地时间：生产写入方（nowLocal / account_invalid）写的是本地时间，
	// parseAccountTime 也按本地解析。用 UTC 会人为造出 8 小时偏移。
	now := time.Now()
	svc.mu.Lock()
	for _, count := range []int{1, 2, 3, 4, 6, 10, 20} {
		svc.accounts[fmt.Sprintf("t-%d", count)] = Account{
			"access_token":    fmt.Sprintf("t-%d", count),
			"status":          StatusAbnormal,
			"invalid_count":   count,
			"last_invalid_at": now.Add(-time.Minute).Format(accountTimeLayout),
			"created_at":      "2020-01-01 00:00:00",
		}
	}
	svc.mu.Unlock()

	due := map[string]bool{}
	for _, tok := range svc.ListAbnormalTokens() {
		due[tok] = true
	}
	for _, count := range []int{1, 2, 3, 4, 6, 10, 20} {
		tok := fmt.Sprintf("t-%d", count)
		expect := count == 1 // 只有 count=1（刚失效）在 1 分钟前失败后仍应重试
		if due[tok] != expect {
			t.Errorf("count=%d due=%v 期望 %v", count, due[tok], expect)
		}
	}
	t.Logf("1 分钟前失败：仅 count=1 被重试（其余在退避窗口内）")

	// 关键：把时间推到很久以后，所有账号都必须重新可重试
	svc.mu.Lock()
	for tok, acc := range svc.accounts {
		acc["last_invalid_at"] = now.Add(-7 * time.Hour).Format(accountTimeLayout)
		svc.accounts[tok] = acc
	}
	svc.mu.Unlock()
	if got := len(svc.ListAbnormalTokens()); got != 7 {
		t.Errorf("7 小时后应全部可重试，实际 %d/7 —— 有账号被永久冻结", got)
	}
	t.Logf("7 小时后：7/7 全部可重试（无永久冻结）")
}
