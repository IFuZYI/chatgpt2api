package api_test

import (
	"context"
	"testing"

	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/basketikun/chatgpt2api/internal/service"
)

// 回归点：SetRunners 从未被调用时，异步图片任务全部报
// "image task runner is not configured"。
//
// 生图页面默认走 /api/image-tasks/* 异步接口，所以表现为「生图界面异常」。
// 这个测试直接断言 runner 已注入 —— 漏调用会立刻红。
func TestImageTaskRunnersInjected(t *testing.T) {
	deps := newTestDeps(t)
	deps.RegisterImageTaskRunners()

	if !deps.Tasks.HasRunners() {
		t.Fatal("image task runners not injected; async image tasks will fail with " +
			"\"image task runner is not configured\"")
	}
}

// 未注入时必须是 false，否则上面的断言就没有意义。
func TestImageTaskRunnersAbsentByDefault(t *testing.T) {
	deps := newTestDeps(t)
	if deps.Tasks.HasRunners() {
		t.Fatal("runners should not be present before RegisterImageTaskRunners is called")
	}
}

// 提交任务后，未注入 runner 时要落到 error 状态而不是卡在 queued。
func TestImageTaskWithoutRunnerFailsFast(t *testing.T) {
	deps := newTestDeps(t)
	task, err := deps.Tasks.Submit(
		"owner", "client-1", "generate", "a cat", "gpt-image-2", "", "auto", "", nil, nil,
	)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if task == nil {
		t.Fatal("task is nil")
	}

	// 等任务进入终态
	deadline := 50
	for i := 0; i < deadline; i++ {
		current := deps.Tasks.GetTask("owner", "client-1")
		if current == nil {
			t.Fatal("task disappeared")
		}
		if service.IsTaskTerminal(stringValueOfTest(current["status"])) {
			if stringValueOfTest(current["status"]) != service.TaskStatusError {
				t.Errorf("status = %v, want error", current["status"])
			}
			if msg := stringValueOfTest(current["error"]); msg == "" {
				t.Error("error message should be set")
			}
			return
		}
		sleepShort()
	}
	t.Fatal("task did not reach terminal state")
}

// runner 拿到的是完整 TaskContext（prompt/model/size/quality/base_url）。
func TestTaskContextFieldsReachRunner(t *testing.T) {
	captured := make(chan service.TaskContext, 1)
	svc := newTaskTestService(t)
	svc.SetRunners(func(ctx service.TaskContext) (service.TaskResult, error) {
		captured <- ctx
		return service.TaskResult{Data: []map[string]any{{"b64_json": "AAA"}}}, nil
	}, func(ctx service.TaskContext) (service.TaskResult, error) {
		captured <- ctx
		return service.TaskResult{Data: []map[string]any{{"b64_json": "BBB"}}}, nil
	})

	if _, err := svc.Submit("owner", "t1", "generate", "a red apple", "gpt-image-2.5",
		"1024x1024", "high", "http://base", nil, nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	select {
	case got := <-captured:
		if got.Prompt != "a red apple" {
			t.Errorf("prompt = %q", got.Prompt)
		}
		if got.Model != "gpt-image-2.5" {
			t.Errorf("model = %q", got.Model)
		}
		if got.Size != "1024x1024" {
			t.Errorf("size = %q", got.Size)
		}
		if got.Quality != "high" {
			t.Errorf("quality = %q", got.Quality)
		}
		if got.BaseURL != "http://base" {
			t.Errorf("baseURL = %q", got.BaseURL)
		}
		if got.Mode != "generate" {
			t.Errorf("mode = %q", got.Mode)
		}
	case <-timeoutChan():
		t.Fatal("runner was not called")
	}
}

var _ = protocol.ImageRequest{}
var _ = context.Background
