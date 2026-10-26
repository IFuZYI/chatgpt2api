package upstream

import (
	"context"
	"testing"
	"time"
)

// applyRequestTimeout 必须把 timeout 转成 ctx deadline。
//
// 为什么需要这个抽象：tls_client.HttpClient 接口无法注入测试 transport，
// 所以「timeout 是否真的生效」只能测这一层。抽出纯函数后可以精确断言，
// 而 doGet/doPostJSON 只是调用它 —— 回归会被这条测试捕获。
func TestApplyRequestTimeoutSetsDeadline(t *testing.T) {
	ctx, cancel := applyRequestTimeout(context.Background(), 5*time.Second)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("没有设置 deadline —— timeout 参数被忽略")
	}
	remaining := time.Until(deadline)
	if remaining > 5*time.Second || remaining < 4*time.Second {
		t.Errorf("deadline 剩余 %v，应约 5s", remaining)
	}
}

// timeout <= 0 时保持「由调用方控制」，不强加 deadline。
func TestApplyRequestTimeoutSkipsNonPositive(t *testing.T) {
	for _, timeout := range []time.Duration{0, -1 * time.Second} {
		ctx, cancel := applyRequestTimeout(context.Background(), timeout)
		if _, ok := ctx.Deadline(); ok {
			t.Errorf("timeout=%v 时不应设置 deadline", timeout)
		}
		cancel()
	}
}

// 父 ctx 已有的 deadline 更早时，不能被延长。
func TestApplyRequestTimeoutKeepsEarlierDeadline(t *testing.T) {
	parent, parentCancel := context.WithTimeout(context.Background(), time.Second)
	defer parentCancel()

	ctx, cancel := applyRequestTimeout(parent, time.Hour)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("没有 deadline")
	}
	if remaining := time.Until(deadline); remaining > 2*time.Second {
		t.Errorf("deadline 被延长到 %v —— 父 ctx 的 1s 应该胜出", remaining)
	}
}
