package service

import (
	"context"
	"sync"
	"testing"
	"time"
)

// image_task 的 Submit 幂等分支与 editable_file_task 有同一个竞态。
//
// 竞态点：image_task.go:486-493
//
//	if existing := s.tasks[key]; existing != nil {
//	    s.mu.Unlock()
//	    return publicTask(existing), nil   // ← 无锁读
//	}
//
// finishTask 在同一把锁下改写该 map，goroutine 写 + 请求线程读 = 数据竞争。
//
// 这个缺陷在 editable_file_task 里已修（editable_task_race_test.go），
// 但两套队列是平行复制的，修一边不会自动修另一边。
func TestImageTaskSubmitIdempotentNoRace(t *testing.T) {
	svc, _ := newCloseTestService(t)

	if _, err := svc.Submit("owner", "race-img-1", "generate", "prompt", "gpt-image-2",
		"", "auto", "http://host", nil, nil); err != nil {
		t.Fatalf("submit: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = svc.Submit("owner", "race-img-1", "generate", "prompt", "gpt-image-2",
				"", "auto", "http://host", nil, nil)
		}()
		go func() {
			defer wg.Done()
			svc.UpdateTask("owner", "race-img-1", map[string]any{"status": TaskStatusSuccess})
		}()
	}
	wg.Wait()

	// 必须等后台 goroutine 收尾再让 TempDir 被删。
	//
	// 不 Close 的话 `t.TempDir()` 的自动清理会与仍在写 data/ 的任务 goroutine
	// 竞争，报 `TempDir RemoveAll cleanup: directory not empty` ——
	// 单次跑通常侥幸躲过，-count=5 就稳定复现。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = svc.Close(ctx)
}
