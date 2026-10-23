package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newCloseTestService 建一个任务服务，data 目录由测试自己管（不依赖 t.TempDir 的
// 自动清理语义，这样能精确断言「关闭后文件已完整落盘」）。
func newCloseTestService(t *testing.T) (*ImageTaskService, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &closeTestConfig{dir: filepath.Join(dir, "data")}
	if err := os.MkdirAll(cfg.dir, 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	return NewImageTaskService(cfg), cfg.dir
}

type closeTestConfig struct {
	dir string
}

func (c *closeTestConfig) DataDir() string         { return c.dir }
func (c *closeTestConfig) ImageRetentionDays() int { return 30 }

// Close 必须等任务 goroutine 真正结束才返回。
//
// 回归点：早期实现没有关闭语义，t.TempDir() 清理时任务还在写
// data/image_tasks.json，表现为 "directory not empty"；真实场景下是
// 进程退出时任务文件被写坏。
func TestImageTaskCloseWaitsForRunningTask(t *testing.T) {
	svc, dataDir := newCloseTestService(t)

	finished := make(chan struct{})
	svc.SetRunners(func(ctx TaskContext) (TaskResult, error) {
		// 模拟一次慢任务：关闭必须等它。
		time.Sleep(120 * time.Millisecond)
		close(finished)
		return TaskResult{Data: []map[string]any{{"b64_json": "AAA"}}}, nil
	}, func(ctx TaskContext) (TaskResult, error) {
		return TaskResult{}, nil
	})

	if _, err := svc.Submit("owner", "t1", "generate", "a cat", "gpt-image-2", "", "auto", "", nil, nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-finished:
	default:
		t.Fatal("Close returned while the task goroutine was still running")
	}

	// 关闭返回后文件必须存在且可解析（说明写完了，不是写了一半）。
	payload, err := os.ReadFile(filepath.Join(dataDir, "image_tasks.json"))
	if err != nil {
		t.Fatalf("read tasks file after Close: %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("tasks file is empty after Close")
	}
}

// 关闭后 Submit 必须明确拒绝，而不是起一个没人等待的 goroutine。
func TestImageTaskCloseRejectsNewSubmit(t *testing.T) {
	svc, _ := newCloseTestService(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := svc.Submit("owner", "t2", "generate", "a cat", "gpt-image-2", "", "auto", "", nil, nil)
	if !errors.Is(err, ErrImageTaskClosed) {
		t.Fatalf("Submit after Close = %v, want ErrImageTaskClosed", err)
	}
}

// 超时必须返回 ctx.Err()，且不能静默 —— 日志里要能看到还在跑的任务数。
func TestImageTaskCloseTimeoutReturnsError(t *testing.T) {
	svc, _ := newCloseTestService(t)

	block := make(chan struct{})
	svc.SetRunners(func(ctx TaskContext) (TaskResult, error) {
		<-block
		return TaskResult{}, nil
	}, func(ctx TaskContext) (TaskResult, error) {
		return TaskResult{}, nil
	})

	if _, err := svc.Submit("owner", "t3", "generate", "a cat", "gpt-image-2", "", "auto", "", nil, nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// 等任务真的进入 running，否则 Close 可能在 goroutine 起步前就超时。
	waitForStatus(t, svc, "owner", "t3", TaskStatusRunning)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := svc.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want context.DeadlineExceeded", err)
	}
	if pending := svc.pendingCount(); pending != 1 {
		t.Errorf("pendingCount = %d, want 1 (used by the timeout log)", pending)
	}

	// 放行任务后必须再等它真正退出：否则 t.TempDir 的清理会和写入者竞争，
	// 偶发 "directory not empty"。这正是本文件要验证的关闭语义，
	// 测试自己也得遵守（第一版漏了这一步，40 次里会红 1~4 次）。
	close(block)
	finalCtx, finalCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finalCancel()
	if err := svc.Close(finalCtx); err != nil {
		t.Fatalf("final Close after unblocking: %v", err)
	}
}

// Close 必须幂等：重复调用不能 panic、不能死锁。
func TestImageTaskCloseIsIdempotent(t *testing.T) {
	svc, _ := newCloseTestService(t)
	svc.SetRunners(func(ctx TaskContext) (TaskResult, error) { return TaskResult{}, nil },
		func(ctx TaskContext) (TaskResult, error) { return TaskResult{}, nil })

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := svc.Close(ctx); err != nil {
			cancel()
			t.Fatalf("Close #%d: %v", i+1, err)
		}
		cancel()
	}
}

// TrackBackground 让 api 层的 resume-poll 也纳入关闭等待。
func TestImageTaskTrackBackgroundWaitsOnClose(t *testing.T) {
	svc, _ := newCloseTestService(t)

	release, ok := svc.TrackBackground()
	if !ok {
		t.Fatal("TrackBackground refused before Close")
	}
	finished := make(chan struct{})
	go func() {
		defer release()
		time.Sleep(80 * time.Millisecond)
		close(finished)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("Close returned before the tracked background goroutine finished")
	}

	// 关闭后不再接受新的后台任务。
	if _, ok := svc.TrackBackground(); ok {
		t.Fatal("TrackBackground should refuse after Close")
	}
}

// waitForStatus 轮询直到任务到达指定状态。
func waitForStatus(t *testing.T, svc *ImageTaskService, ownerID, taskID, status string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task := svc.GetTask(ownerID, taskID)
		if task != nil && stringValue(task["status"]) == status {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s never reached status %q", taskID, status)
}

// 关闭时必须**先取消**长跑的后台工作（账号刷新 30 分钟 ctx），再等它们退出。
// 只等不取消会让关闭必然超时。
func TestImageTaskCloseCancelsBackgroundWork(t *testing.T) {
	svc, _ := newCloseTestService(t)

	workCtx, finish, ok := svc.StartBackground(context.Background(), 30*time.Minute)
	if !ok {
		t.Fatal("StartBackground refused before Close")
	}
	stopped := make(chan struct{})
	go func() {
		defer finish()
		defer close(stopped)
		<-workCtx.Done() // 只有被取消才会走到这里
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-stopped:
	default:
		t.Fatal("background work was not cancelled by Close")
	}
	if workCtx.Err() == nil {
		t.Error("background ctx should be cancelled after Close")
	}
}

// 关闭后不再接受新的后台工作。
func TestImageTaskStartBackgroundRejectsAfterClose(t *testing.T) {
	svc, _ := newCloseTestService(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, ok := svc.StartBackground(context.Background(), time.Minute); ok {
		t.Fatal("StartBackground should refuse after Close")
	}
}

// finish 必须可重复调用（defer 与显式清理同时出现时不能 double Done）。
func TestBackgroundWorkFinishIsIdempotent(t *testing.T) {
	work := NewBackgroundWork()
	_, finish, ok := work.Start(context.Background(), time.Minute)
	if !ok {
		t.Fatal("Start refused")
	}
	finish()
	finish()
	if pending := work.pending(); pending != 0 {
		t.Errorf("pending = %d, want 0", pending)
	}
}
