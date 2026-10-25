package api_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/IFuZYI/chatgpt2api/internal/service"
)

// 回归点：RegisterEditableFileTaskRunner 从未被调用时，所有 PPT/PSD 任务都会
// 失败并报 "editable file task runner is not configured"。
//
// 与 image task 的 RegisterImageTaskRunners 是同一类装配遗漏，
// 这里直接断言 runner 与号池都已注入 —— 漏调用会立刻红。
func TestEditableFileTaskRunnerInjected(t *testing.T) {
	deps := newTestDeps(t)
	deps.EditableTasks = service.NewEditableFileTaskService(deps.Config.DataDir())
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := deps.EditableTasks.Close(closeCtx); err != nil {
			t.Errorf("close editable tasks: %v", err)
		}
	})
	deps.EditableUpstream = func(context.Context, string) (protocol.EditableFileUpstream, func(), error) {
		return nil, nil, errors.New("not used")
	}

	deps.RegisterEditableFileTaskRunner()
	if !deps.EditableTasks.HasRunner() {
		t.Fatal("editable file task runner not injected; PPT/PSD tasks will fail with " +
			"\"editable file task runner is not configured\"")
	}
}

// 未注入时必须是 false，否则上面的断言没有意义。
func TestEditableFileTaskRunnerAbsentByDefault(t *testing.T) {
	deps := newTestDeps(t)
	deps.EditableTasks = service.NewEditableFileTaskService(deps.Config.DataDir())
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = deps.EditableTasks.Close(closeCtx)
	})
	if deps.EditableTasks.HasRunner() {
		t.Fatal("runner should not be present before RegisterEditableFileTaskRunner is called")
	}
}

// 上游工厂未配置时，任务落到 error 而不是卡在 queued。
func TestEditableFileTaskWithoutUpstreamFailsFast(t *testing.T) {
	deps := newTestDeps(t)
	deps.EditableTasks = service.NewEditableFileTaskService(deps.Config.DataDir())
	deps.Accts.AddAccountItems([]map[string]any{editableAccount()}, nil)
	deps.RegisterEditableFileTaskRunner()
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = deps.EditableTasks.Close(closeCtx)
	})

	if _, err := deps.EditableTasks.Submit("owner", "no-upstream", "", "ppt", "p", nil, ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task := deps.EditableTasks.GetTask("owner", "no-upstream")
		if task != nil && service.IsTaskTerminal(stringValueOfTest(task["status"])) {
			if stringValueOfTest(task["status"]) != service.TaskStatusError {
				t.Fatalf("status = %v, want error", task["status"])
			}
			if stringValueOfTest(task["error"]) != "editable file upstream factory is not configured" {
				t.Fatalf("error = %q", stringValueOfTest(task["error"]))
			}
			return
		}
		sleepShort()
	}
	t.Fatal("task did not reach terminal state")
}
