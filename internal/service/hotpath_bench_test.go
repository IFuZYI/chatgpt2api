package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/config"
	"github.com/IFuZYI/chatgpt2api/internal/storage"
)

// 关键热路径的基准：每个上游请求都会走这几条。
// 它们的耗时直接决定吞吐上限（尤其是账号选择，每个请求一次）。

// benchService 直接构造 AccountService，不复用 newTestAccountService ——
// 后者签名要 *testing.T（用 t.TempDir/t.Setenv），基准里拿不到。
func benchService(b *testing.B, n int) *AccountService {
	b.Helper()
	items := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, map[string]any{
			"access_token": fmt.Sprintf("at-%04d", i),
			"email":        fmt.Sprintf("u%04d@example.test", i),
			"type":         "free",
			"status":       StatusNormal,
			"quota":        100,
		})
	}
	dir := b.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auth-key":"bench"}`), 0o644); err != nil {
		b.Fatal(err)
	}
	cfg, err := config.NewStore(dir)
	if err != nil {
		b.Fatal(err)
	}
	backend, err := storage.NewFromEnv(cfg.DataDir())
	if err != nil {
		b.Fatal(err)
	}
	logs, err := NewLogService(filepath.Join(cfg.DataDir(), "logs.jsonl"))
	if err != nil {
		b.Fatal(err)
	}
	svc := NewAccountService(backend, cfg, logs)
	svc.AddAccountItems(items, nil)
	return svc
}

// 文本链路选号：每个 /v1/chat/completions 请求一次。
func BenchmarkGetTextAccessToken(b *testing.B) {
	for _, size := range []int{10, 100, 300} {
		b.Run(fmt.Sprintf("accounts=%d", size), func(b *testing.B) {
			svc := benchService(b, size)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := svc.GetTextAccessToken(nil, nil, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// 图片链路选号：含槽位获取与释放。
func BenchmarkAcquireImageSlot(b *testing.B) {
	for _, size := range []int{10, 100, 300} {
		b.Run(fmt.Sprintf("accounts=%d", size), func(b *testing.B) {
			svc := benchService(b, size)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				token, err := svc.AcquireImageSlot(context.Background(), SlotOptions{})
				if err != nil {
					b.Fatal(err)
				}
				svc.ReleaseImageSlot(token)
			}
		})
	}
}

// ProxyFor：每个上游请求都会调用（含锁内读 map）。
func BenchmarkProxyFor(b *testing.B) {
	for _, size := range []int{10, 100, 300} {
		b.Run(fmt.Sprintf("accounts=%d", size), func(b *testing.B) {
			svc := benchService(b, size)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = svc.ProxyFor("at-0000")
			}
		})
	}
}

// 状态选择器：watcher 每轮遍历全池。
func BenchmarkListMaintainableTokens(b *testing.B) {
	for _, size := range []int{100, 300, 1000} {
		b.Run(fmt.Sprintf("accounts=%d", size), func(b *testing.B) {
			svc := benchService(b, size)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = svc.ListMaintainableTokens()
			}
		})
	}
}

// 失效观察：失败路径的写盘与决策。
func BenchmarkRecordInvalidTokenSeen(b *testing.B) {
	svc := benchService(b, 300)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		svc.RecordInvalidTokenSeen("at-0000", "refresh", "401", true)
	}
}
