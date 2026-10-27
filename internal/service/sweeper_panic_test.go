package service

import (
	"sync/atomic"
	"testing"
	"time"
)

// 后台清理 goroutine 必须能从 panic 中恢复。
//
// 现状：StartProgressSweeper 的 goroutine 没有任何 recover。Go 里
// **未捕获的 goroutine panic 会终止整个进程**（不是只死那个 goroutine），
// 而 watcher tick 早就加了同样的保护（见 watcher.go 的 recover 注释）。
// 这里逻辑虽简单，但一旦将来有人在 SweepStaleProgress 里加了会 panic 的
// 操作（nil map、类型断言、除零），整台服务会直接退出。
func TestSweeperRecoversFromPanic(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)

	// 用极短间隔，确保测试期间至少跑一轮。
	// 注入的清理函数第一轮 panic，第二轮起正常 —— 这样既能验证
	// 「panic 没杀死循环」，也能验证「循环还在继续」。
	var calls atomic.Int32
	stop := svc.startProgressSweeperWithInterval(10*time.Millisecond, func() {
		n := calls.Add(1)
		if n == 1 {
			panic("sweeper blew up")
		}
	})
	defer stop()

	// 等到第二轮真的跑过：证明 panic 之后循环仍然存活。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("panic 之后清理循环没有继续运行（calls=%d）—— 缺少 recover", calls.Load())
}

// 停止函数必须幂等（重复调用不能 panic）。
func TestSweeperStopIsIdempotent(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)
	stop := svc.startProgressSweeperWithInterval(10*time.Millisecond, func() {})
	stop()
	stop() // 第二次不应 panic（close of closed channel）
}
