package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// 并发选号：验证 RWMutex 在高并发下不会成为瓶颈。
// 这是真实场景（多个请求同时选号）。
func BenchmarkGetTextAccessTokenParallel(b *testing.B) {
	for _, size := range []int{100, 300} {
		b.Run(fmt.Sprintf("accounts=%d", size), func(b *testing.B) {
			svc := benchService(b, size)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := svc.GetTextAccessToken(nil, nil, false); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// 并发图片槽位获取/释放。
func BenchmarkAcquireImageSlotParallel(b *testing.B) {
	svc := benchService(b, 300)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			token, err := svc.AcquireImageSlot(context.Background(), SlotOptions{})
			if err != nil {
				b.Fatal(err)
			}
			svc.ReleaseImageSlot(token)
		}
	})
}

// 并发 ProxyFor（每请求调用，只读）。
func BenchmarkProxyForParallel(b *testing.B) {
	svc := benchService(b, 300)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = svc.ProxyFor("at-0000")
		}
	})
}

var _ = sync.WaitGroup{}
