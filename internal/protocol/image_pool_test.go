package protocol

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// poolFake 给每张图返回固定结果，并记录并发度。
type poolFake struct {
	mu       sync.Mutex
	acquired []int
	released int32
	peak     int32
	current  int32
	// failIndexes 指定哪些索引失败
	failIndexes map[int]bool
	// delay 模拟上游耗时
	delay time.Duration
}

func (f *poolFake) Acquire(ctx context.Context, index int) (*ImageGenerator, func(), error) {
	f.mu.Lock()
	f.acquired = append(f.acquired, index)
	f.mu.Unlock()

	now := atomic.AddInt32(&f.current, 1)
	for {
		peak := atomic.LoadInt32(&f.peak)
		if now <= peak || atomic.CompareAndSwapInt32(&f.peak, peak, now) {
			break
		}
	}

	shouldFail := f.failIndexes[index]

	generator := &ImageGenerator{
		Upstream: &imageFake{
			fakeImageUpstream: fakeImageUpstream{},
			downloadBytes:     []byte("PNG"),
			urls:              []string{"https://example.test/i.png"},
		},
		Config: fastGenConfig(),
	}
	// 用 SaveImage 之外的钩子模拟失败/延迟：直接换成自定义 Upstream 不现实，
	// 所以这里通过包装 Generate 的行为来做。
	if shouldFail || f.delay > 0 {
		generator = nil
	}

	release := func() {
		atomic.AddInt32(&f.current, -1)
		atomic.AddInt32(&f.released, 1)
	}

	if generator == nil {
		// 返回一个必然失败或延迟的 generator
		return &ImageGenerator{
			Upstream: &delayedImageFake{
				inner:      &imageFake{downloadBytes: []byte("PNG"), urls: []string{"u"}},
				delay:      f.delay,
				shouldFail: shouldFail,
			},
			Config: fastGenConfig(),
		}, release, nil
	}
	return generator, release, nil
}

// delayedImageFake 可模拟失败与延迟。
type delayedImageFake struct {
	inner      *imageFake
	delay      time.Duration
	shouldFail bool
}

func (d *delayedImageFake) ChatRequirements(ctx context.Context) (Requirements, error) {
	return Requirements{Token: "t"}, nil
}

func (d *delayedImageFake) DoStream(ctx context.Context, path string, payload []byte, req Requirements) (readCloser, error) {
	if d.shouldFail {
		return nil, errors.New("simulated upstream failure")
	}
	time.Sleep(d.delay)
	lines := []string{
		"data: " + `{"conversation_id":"c","type":"server_ste_metadata","metadata":{"turn_use_case":"image gen"}}`,
		"data: [DONE]",
	}
	return newStringReadCloser(joinStrings(lines) + "\n"), nil
}

// StreamImage 必须自己实现：它承载了 shouldFail / delay 语义。
// 内嵌 streamStub 会静默返回空流，让「全部失败」用例失去意义。
func (d *delayedImageFake) StreamImage(ctx context.Context, request ImageStreamRequest) (<-chan string, <-chan error, error) {
	if d.shouldFail {
		return nil, nil, errors.New("simulated upstream failure")
	}
	time.Sleep(d.delay)
	lines := []string{
		`{"conversation_id":"c","type":"server_ste_metadata","metadata":{"turn_use_case":"image gen"}}`,
		"[DONE]",
	}
	payloads := make(chan string, len(lines))
	errs := make(chan error, 1)
	for _, line := range lines {
		payloads <- line
	}
	close(payloads)
	close(errs)
	return payloads, errs, nil
}

func (d *delayedImageFake) Authenticated() bool { return true }
func (d *delayedImageFake) UploadImage(payload []byte, filename, mime string) (map[string]any, error) {
	return map[string]any{"file_id": "f"}, nil
}
func (d *delayedImageFake) GetConversation(ctx context.Context, id string) (map[string]any, error) {
	return conversationWithImage("img1"), nil
}
func (d *delayedImageFake) QueryTasks(ctx context.Context, id string) ([]map[string]any, error) {
	return nil, nil
}
func (d *delayedImageFake) ResolveImageURLs(ctx context.Context, id string, f, s []string) ([]string, error) {
	return []string{"https://example.test/i.png"}, nil
}
func (d *delayedImageFake) DownloadImage(ctx context.Context, url string) ([]byte, error) {
	return []byte("PNG"), nil
}
func (d *delayedImageFake) FindConversationByPrompt(ctx context.Context, prompt string, since time.Time) (string, error) {
	return "", nil
}
func (d *delayedImageFake) DeleteConversation(ctx context.Context, id string) error { return nil }

// n=1 时不并发。
func TestImagePoolSingleRequest(t *testing.T) {
	fake := &poolFake{}
	pool := &ImagePool{Parallel: true, Acquire: fake.Acquire}
	result, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.acquired) != 1 {
		t.Errorf("acquired = %v, want 1", fake.acquired)
	}
	if len(result.Outputs) == 0 {
		t.Error("expected outputs")
	}
}

// 并行模式下多张图应该并发（峰值 > 1）。
func TestImagePoolParallelConcurrency(t *testing.T) {
	fake := &poolFake{delay: 50 * time.Millisecond}
	pool := &ImagePool{Parallel: true, Acquire: fake.Acquire}
	result, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Outputs) == 0 {
		t.Fatal("expected outputs")
	}
	if atomic.LoadInt32(&fake.peak) < 2 {
		t.Errorf("peak concurrency = %d, want >= 2 (parallel mode)", fake.peak)
	}
}

// 串行模式下并发度必须为 1。
func TestImagePoolSerialConcurrency(t *testing.T) {
	fake := &poolFake{delay: 10 * time.Millisecond}
	pool := &ImagePool{Parallel: false, Acquire: fake.Acquire}
	if _, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 3}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(&fake.peak) != 1 {
		t.Errorf("peak concurrency = %d, want 1 (serial mode)", fake.peak)
	}
}

// 槽位必须成对释放（泄漏检测）。
func TestImagePoolReleasesSlots(t *testing.T) {
	fake := &poolFake{}
	pool := &ImagePool{Parallel: true, Acquire: fake.Acquire}
	if _, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 4}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&fake.released); got != 4 {
		t.Errorf("released = %d, want 4 (slot leak)", got)
	}
	if got := atomic.LoadInt32(&fake.current); got != 0 {
		t.Errorf("current = %d, want 0 (slot leak)", got)
	}
}

// 部分失败：成功的照常返回，失败记入 Failures，不抛错。
func TestImagePoolPartialFailure(t *testing.T) {
	fake := &poolFake{failIndexes: map[int]bool{2: true}}
	pool := &ImagePool{Parallel: true, Acquire: fake.Acquire}
	result, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 3})
	if err != nil {
		t.Fatalf("partial failure should not error: %v", err)
	}
	if len(result.Failures) != 1 || result.Failures[2] == "" {
		t.Errorf("failures = %v, want index 2", result.Failures)
	}
	if len(result.Outputs) == 0 {
		t.Error("successful outputs should still be returned")
	}
}

// 全部失败：要返回错误。
func TestImagePoolAllFailures(t *testing.T) {
	fake := &poolFake{failIndexes: map[int]bool{1: true, 2: true}}
	pool := &ImagePool{Parallel: true, Acquire: fake.Acquire}
	_, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 2})
	if err == nil {
		t.Fatal("expected error when all images fail")
	}
}

func TestCollectImageOutputs(t *testing.T) {
	outputs := []ImageOutput{
		{Kind: ImageKindProgress, Text: "step1"},
		{Kind: ImageKindProgress, Text: "step2"},
		{Kind: ImageKindResult, Data: []map[string]any{{"url": "u1"}}},
		{Kind: ImageKindResult, Data: []map[string]any{{"url": "u2"}}},
		{Kind: ImageKindResult, AccountEmail: "a@b.c"},
	}
	result := CollectImageOutputs(outputs)
	data, _ := result["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data len = %d, want 2", len(data))
	}
	if result["_account_email"] != "a@b.c" {
		t.Errorf("account email missing: %v", result)
	}
	// 有 data 时不应出现 message
	if _, exists := result["message"]; exists {
		t.Error("message should be absent when data exists")
	}
}

// 没有 data 时，progress 文本拼起来作为 message。
func TestCollectImageOutputsProgressAsMessage(t *testing.T) {
	result := CollectImageOutputs([]ImageOutput{
		{Kind: ImageKindProgress, Text: "a"},
		{Kind: ImageKindProgress, Text: "b"},
	})
	if result["message"] != "ab" {
		t.Errorf("message = %v, want 'ab'", result["message"])
	}
}

// message kind 优先于 progress 拼接。
func TestCollectImageOutputsMessageWins(t *testing.T) {
	result := CollectImageOutputs([]ImageOutput{
		{Kind: ImageKindProgress, Text: "progress"},
		{Kind: ImageKindMessage, Text: "final message"},
	})
	if result["message"] != "final message" {
		t.Errorf("message = %v, want 'final message'", result["message"])
	}
}
