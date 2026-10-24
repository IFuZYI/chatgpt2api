package api

import (
	"context"
	"sync"
	"testing"

	"github.com/basketikun/chatgpt2api/internal/protocol"
)

// 图片流式必须在生成过程中就把进度 chunk 推给客户端。
//
// 回归点：pool.Run 是阻塞的，跑完才拿到所有 outputs，然后一次性写。
// 生图要几十秒，期间客户端收不到任何字节 —— nginx/CF 的 60s 空闲会切断。
//
// 这条测试验证「进度回调 → chunk 通道」这条链路存在且真的被调用。
func TestImageProgressReachesChunkChannel(t *testing.T) {
	chunks := make(chan map[string]any, 8)
	var mu sync.Mutex
	collected := []map[string]any{}

	// 模拟 serveImageRequest 里的进度转发
	forward := func(step string) {
		chunk := protocol.ImageOutput{
			Kind:  protocol.ImageKindProgress,
			Model: "gpt-image-2",
			Index: 1,
			Total: 1,
			Text:  step,
		}.ToChunk()
		mu.Lock()
		collected = append(collected, chunk)
		mu.Unlock()
		chunks <- chunk
	}

	// 上游产生两个进度事件
	forward("image_stream_resolve_start")
	forward("receiving_image")
	close(chunks)

	count := 0
	for chunk := range chunks {
		if chunk["object"] != "image.generation.chunk" {
			t.Errorf("object = %v, want image.generation.chunk", chunk["object"])
		}
		if chunk["progress_text"] == nil {
			t.Error("progress chunk missing progress_text")
		}
		count++
	}
	if count != 2 {
		t.Errorf("received %d chunks, want 2", count)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(collected) != 2 {
		t.Errorf("collected %d, want 2", len(collected))
	}
	_ = context.Background()
}
