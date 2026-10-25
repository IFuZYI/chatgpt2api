package service

import (
	"testing"
	"time"
)

// created_at 必须与账号其他时间字段用同一时区写。
//
// 回归点：AddAccounts 曾经用 nowString()（UTC）写 created_at，而
// shouldDeferInvalidTokenLocked 的「新号宽限」用 ParseInLocation(time.Local) 读。
// 后果双向：
//
//	UTC+X 部署 → 新号看起来已过 X 小时 → 宽限对刚导入的号永不生效
//	UTC-X 部署 → created_at 落在未来 → 宽限恒真 → 账号永不可删
//
// 真正修好需要改**写入点**（AddAccounts），而不是 normalizeAccount 的补空分支 ——
// 后者只对空值生效，新账号早在 AddAccounts 里就被写成 UTC 了。
func TestCreatedAtUsesLocalTimezone(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)
	before := time.Now()
	svc.AddAccounts([]string{"at-tz-1"}, "")
	account := svc.GetAccount("at-tz-1")
	if account == nil {
		t.Fatal("账号未创建")
	}

	raw := stringValue(account["created_at"])
	parsed, err := time.ParseInLocation(accountTimeLayout, raw, time.Local)
	if err != nil {
		t.Fatalf("created_at 无法按本地时区解析: %q", raw)
	}
	// 与真实「现在」的偏差必须在一分钟内（写本地时间才可能）。
	skew := parsed.Sub(before)
	if skew < -time.Minute || skew > time.Minute {
		t.Fatalf("created_at 偏差 %v（TZ=%s，写入值 %q）—— 写入时区与解析时区不一致",
			skew.Round(time.Second), time.Now().Location(), raw)
	}

	// 新号宽限必须生效（shouldDefer 是未导出方法，测试同包可直接调）
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	if !svc.shouldDeferInvalidTokenLocked(account, time.Now()) {
		t.Error("新号宽限未生效：刚创建的账号会被直接删除")
	}
}
