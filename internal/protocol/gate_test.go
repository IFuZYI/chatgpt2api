package protocol

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 合成闸门必须**每张图**取一次，而不是每个请求一次。
//
// 回归点：acquireComposite() 原本在 for 循环之外调用，而循环内每张图
// 各自最多 20M 像素（≈271 MiB 峰值）。images 数量在 API 层没有上限
// （16 MiB body 足以塞进多张「小文件、大 IHDR」的 PNG），于是闸门=1 时
// 单请求仍可产生 N×271 MiB —— 闸门与像素预算的联动假设不成立。
//
// 这条测试验证：同时进行的合成「张数」不超过闸门容量。
func TestCompositeGateLimitsPerImageNotPerRequest(t *testing.T) {
	capacity := maxConcurrentComposites
	if capacity < 1 {
		t.Skip("闸门容量异常")
	}

	var inFlight int32
	var peak int32
	var wg sync.WaitGroup

	// 模拟「一个请求处理多张图」：每次进入临界区都记一次并发数
	const imagesPerRequest = 4
	const requests = 3

	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < imagesPerRequest; j++ {
				release := acquireComposite()
				current := atomic.AddInt32(&inFlight, 1)
				for {
					old := atomic.LoadInt32(&peak)
					if current <= old || atomic.CompareAndSwapInt32(&peak, old, current) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				atomic.AddInt32(&inFlight, -1)
				release()
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&peak); got > int32(capacity) {
		t.Errorf("同时进行的合成数达到 %d，超过闸门容量 %d —— 闸门按请求而非按图限流",
			got, capacity)
	}
}
