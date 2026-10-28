package service

import (
	"fmt"
	"sync"
	"testing"
)

// 池化的候选切片必须在并发下正确，且不泄漏 token 引用。
//
// 这是每个 /v1/chat/completions 都走的路径。改成 sync.Pool 后有两个
// 新风险：① 缓冲被两个 goroutine 同时拿到（Pool 保证不会，但要有测试）；
// ② 归还时没清空，导致池里钉住一批 token 字符串（内存泄漏）。
func TestGetTextAccessTokenConcurrentPoolSafety(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)
	items := make([]map[string]any, 0, 50)
	for i := 0; i < 50; i++ {
		items = append(items, map[string]any{
			"access_token": fmt.Sprintf("at-%03d", i),
			"type":         "free",
			"status":       StatusNormal,
			"quota":        10,
		})
	}
	svc.AddAccountItems(items, nil)

	const workers, iterations = 16, 200
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				token, err := svc.GetTextAccessToken(nil, nil, false)
				if err != nil {
					errs <- err
					return
				}
				if token == "" {
					errs <- fmt.Errorf("拿到空 token")
					return
				}
				if len(token) < 3 || token[:3] != "at-" {
					errs <- fmt.Errorf("拿到损坏的 token: %q", token)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发调用失败: %v", err)
	}
}

// 生产路径归还的缓冲必须清空底层数组，不能把 token 引用留在池里。
//
// 断言方式：走一次真实的 GetTextAccessToken（它会借出并归还缓冲），
// 然后从池里取出那个缓冲，检查底层数组的元素是否已被清零。
//
// 只写 `s[:0]` 是不够的：长度归零但底层数组仍持有字符串引用，
// GC 无法回收 —— 池里会一直钉住上一轮全池的 token。
func TestTextCandidateBufferClearedOnPut(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)
	svc.AddAccountItems([]map[string]any{
		{"access_token": "secret-token", "type": "free", "status": StatusNormal, "quota": 10},
	}, nil)

	// 先清空池里可能存在的旧缓冲，保证接下来借到的是「本次放进去的那个」。
	for {
		if v, _ := svc.textCandidateBuf.Get().(*[]string); v == nil {
			break
		}
	}

	// 塞一个**只有这一个**缓冲的池，内容已知。
	seeded := []string{"secret-token"}
	svc.textCandidateBuf.Put(&seeded)

	// 走一次真实的选号：它会借出这个缓冲、写入 token、归还前 clear。
	if _, err := svc.GetTextAccessToken(nil, nil, false); err != nil {
		t.Fatalf("选号失败: %v", err)
	}

	// 现在池里应该只有它，且底层数组已被清零。
	got, _ := svc.textCandidateBuf.Get().(*[]string)
	if got == nil {
		t.Skip("池被 GC 清空，无法观察（环境限制，不是实现缺陷）")
	}
	full := (*got)[:cap(*got)]
	for i, v := range full {
		if v != "" {
			t.Errorf("底层数组[%d] 仍持有 %q —— 归还前没有清零", i, v)
		}
	}
}
