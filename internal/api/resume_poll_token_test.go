package api_test

import (
	"testing"
)

// resume-poll 必须从任务记录读 token，不能传空串。
//
// 回归点：之前 accessToken := ""，上游对空 Authorization 返回 401，
// 「继续等待」永远失败。Python 靠匿名 backend 绕过，但匿名链路已坏
// （ADR-0008），所以 Go 自己存 token。
//
// 这条测试走真实链路：Submit → RecordTaskAccessToken → 读回。
// 完整的 HTTP resume-poll 需要实现 ImageUpstream 全部方法（10+ 个），
// 成本高于收益；token 的存取本身由 service 层测试覆盖。
func TestTaskTokenRoundTripThroughService(t *testing.T) {
	deps := newTestDeps(t)

	if _, err := deps.Tasks.Submit("owner", "task-rt", "generate", "p", "gpt-image-2", "", "auto", "", nil, nil); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// 没记 token 时是空（resume-poll 会据此报错，不静默 401）
	if got := deps.Tasks.TaskAccessToken("owner", "task-rt"); got != "" {
		t.Errorf("before recording: %q, want empty", got)
	}

	deps.Tasks.RecordTaskAccessToken("owner", "task-rt", "tok-xyz")

	if got := deps.Tasks.TaskAccessToken("owner", "task-rt"); got != "tok-xyz" {
		t.Errorf("after recording: %q, want tok-xyz", got)
	}
}
