package protocol

import (
	"testing"
	"time"
)

// chunk 的 created 不能是 0。
//
// 回归点：Python 的 ImageOutput.created 是
// `field(default_factory=lambda: int(time.time()))`（conversation.py:330），
// 构造时自动赋当前时间。Go 的 int64 零值是 0，而所有构造点都没设它，
// 于是每个 chunk 的 created 都是 0 —— 前端按时间排序/去重会全部失效。
func TestToChunkFillsCreatedWhenZero(t *testing.T) {
	before := time.Now().Unix()

	chunk := ImageOutput{Kind: ImageKindProgress, Model: "gpt-image-2", Text: "准备中"}.ToChunk()

	created, ok := chunk["created"].(int64)
	if !ok {
		t.Fatalf("created = %T, want int64", chunk["created"])
	}
	if created == 0 {
		t.Fatal("created = 0, want current unix time (Python default_factory)")
	}
	if created < before-2 || created > time.Now().Unix()+2 {
		t.Errorf("created = %d, want near %d", created, before)
	}
}

// 显式赋值的 created 必须被保留（不能总是覆盖成当前时间）。
func TestToChunkKeepsExplicitCreated(t *testing.T) {
	const explicit int64 = 1700000000

	chunk := ImageOutput{Kind: ImageKindResult, Created: explicit}.ToChunk()

	if got, _ := chunk["created"].(int64); got != explicit {
		t.Errorf("created = %d, want %d (explicit value must be preserved)", got, explicit)
	}
}
