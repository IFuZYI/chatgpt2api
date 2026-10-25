package protocol

import (
	"context"
	"testing"
)

func okAcquire(ctx context.Context, index int) (*ImageGenerator, func(), error) {
	return &ImageGenerator{}, func() {}, nil
}

// n=1（chat 与所有异步任务的默认路径）上的 panic 不能让进程退出。
//
// 回归点：recover 原先只在 runParallel 的 goroutine 里，runSingle 与
// runSerial 是**同步**调用 —— panic 穿透到调用方，而调用方（streamImageChat
// 与两个 runTask）都是裸 `go` 起的，整进程退出。
// gin.Recovery 只管 handler goroutine，救不到。
func TestRunRecoversPanicOnSinglePath(t *testing.T) {
	pool := &ImagePool{
		Acquire: okAcquire,
		Generate: func(ctx context.Context, g *ImageGenerator, req ImageRequest) ([]ImageOutput, error) {
			panic("malformed upstream payload")
		},
	}
	out, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1})
	if err == nil {
		t.Fatal("panic 未转成 error：调用方会拿到看起来成功的结果")
	}
	if len(out.Outputs) != 0 {
		t.Errorf("panic 后不应有 outputs，实际 %d", len(out.Outputs))
	}
	t.Logf("n=1 panic 已收住: %v", err)
}

// 串行路径同理。
func TestRunRecoversPanicOnSerialPath(t *testing.T) {
	pool := &ImagePool{
		Parallel: false,
		Acquire:  okAcquire,
		Generate: func(ctx context.Context, g *ImageGenerator, req ImageRequest) ([]ImageOutput, error) {
			panic("boom")
		},
	}
	if _, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 3}); err == nil {
		t.Fatal("串行路径的 panic 未收住")
	}
	t.Log("串行 panic 已收住")
}

// 正常路径不受 recover 影响。
func TestRunNormalPathUnaffectedByRecover(t *testing.T) {
	pool := &ImagePool{
		Acquire: okAcquire,
		Generate: func(ctx context.Context, g *ImageGenerator, req ImageRequest) ([]ImageOutput, error) {
			return []ImageOutput{{Kind: "image", Index: 1, Total: 1}}, nil
		},
	}
	out, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1})
	if err != nil {
		t.Fatalf("正常路径返回 error: %v", err)
	}
	if len(out.Outputs) != 1 {
		t.Errorf("outputs=%d，应为 1", len(out.Outputs))
	}
}
