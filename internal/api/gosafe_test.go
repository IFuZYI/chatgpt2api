package api

import (
	"sync"
	"testing"
)

// 后台 goroutine 里的 panic 必须被 recover。
//
// 回归点：gin.Recovery() 只兜 handler goroutine。刷新/重登/导入等
// `go func()` 在 handler 返回后继续跑，其中任何一个 panic 都会终止
// 整个进程 —— 所有在途任务与刷新一起丢。ImageTaskService.runTask
// 已因同样理由自己加了 recover，api 层这几处没跟上。
func TestGoSafeRecoversPanic(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)

	// 不应 panic 到测试进程
	goSafe(nil, "test", func() {
		defer wg.Done()
		panic("boom")
	})
	wg.Wait()
}

// 正常函数照常执行。
func TestGoSafeRunsFunction(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	ran := false

	goSafe(nil, "test", func() {
		defer wg.Done()
		ran = true
	})
	wg.Wait()

	if !ran {
		t.Error("函数没有执行")
	}
}
