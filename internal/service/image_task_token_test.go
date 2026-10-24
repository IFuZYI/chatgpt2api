package service

import (
	"context"
	"testing"
	"time"
)

// closeTaskServiceForTest 在测试结束时关闭任务服务。
//
// 不关的话 Submit 起的任务 goroutine 会在测试结束后继续写
// data/image_tasks.json，t.TempDir() 清理时撞上 "directory not empty"，
// 表现为随机 FAIL（基线 f92c882 同样复现，是既有 flake，与业务无关）。
func closeTaskServiceForTest(t *testing.T, svc *ImageTaskService) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := svc.Close(ctx); err != nil {
			t.Errorf("close image task service: %v", err)
		}
	})
}

// 任务完成后必须记下「用的是哪个账号的 token」。
//
// 回归点：resume-poll 之前用空 token 构造上游，必 401。
// Python 靠匿名 backend（openai_backend_api.py:165）绕过，但匿名对话
// 链路已实测不可用（ADR-0008，上游 403/500）—— 也就是说 Python 的
// 「继续等待」实际也是坏的。Go 要真正可用必须自己存 token。
func TestTaskRecordsAccessToken(t *testing.T) {
	svc, _ := newCloseTestService(t)
	closeTaskServiceForTest(t, svc)

	task, err := svc.Submit("owner", "task-1", "generate", "prompt", "gpt-image-2",
		"", "auto", "http://localhost", nil, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task == nil {
		t.Fatal("nil task")
	}

	// 记 token（真实链路在 acquireImageGenerator 拿到 token 后调用）
	svc.RecordTaskAccessToken("owner", "task-1", "tok-abc")

	got := svc.TaskAccessToken("owner", "task-1")
	if got != "tok-abc" {
		t.Errorf("access token = %q, want %q", got, "tok-abc")
	}
}

// 未知任务返回空串（不 panic）。
func TestTaskAccessTokenMissingReturnsEmpty(t *testing.T) {
	svc, _ := newCloseTestService(t)
	closeTaskServiceForTest(t, svc)
	if got := svc.TaskAccessToken("owner", "nonexistent"); got != "" {
		t.Errorf("access token = %q, want empty", got)
	}
}

// token 必须落盘（进程重启后 resume-poll 仍能用）。
func TestTaskAccessTokenPersists(t *testing.T) {
	svc, dir := newCloseTestService(t)
	closeTaskServiceForTest(t, svc)
	if _, err := svc.Submit("owner", "task-2", "generate", "p", "gpt-image-2", "", "auto", "", nil, nil); err != nil {
		t.Fatalf("submit: %v", err)
	}
	svc.RecordTaskAccessToken("owner", "task-2", "tok-persist")

	// 从磁盘重新加载（同一个 data 目录）
	reloaded := NewImageTaskService(&closeTestConfig{dir: dir})
	closeTaskServiceForTest(t, reloaded)
	if got := reloaded.TaskAccessToken("owner", "task-2"); got != "tok-persist" {
		t.Errorf("after reload access token = %q, want %q", got, "tok-persist")
	}
}
