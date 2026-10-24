package service

import (
	"sync"
	"testing"
)

// Submit 的幂等分支在 Unlock 之后读 map，而 finishTask 在 Lock 下写同一个 map。
//
// 竞态点：editable_file_task.go:198-200
//
//	if existing := s.tasks[key]; existing != nil {
//	    s.mu.Unlock()
//	    return publicEditableTask(existing), nil   // ← 无锁读
//	}
//
// 这条测试并发打 Submit + finishTask，-race 下能暴露。
func TestEditableTaskSubmitIdempotentNoRace(t *testing.T) {
	svc := newEditableTestService(t)

	if _, err := svc.Submit("owner", "race-1", "key", "ppt", "prompt", nil, ""); err != nil {
		t.Fatalf("submit: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = svc.Submit("owner", "race-1", "key", "ppt", "prompt", nil, "")
		}()
		go func() {
			defer wg.Done()
			svc.finishTask(taskKey("owner", "race-1"), EditableFileTaskResult{}, nil)
		}()
	}
	wg.Wait()
}
