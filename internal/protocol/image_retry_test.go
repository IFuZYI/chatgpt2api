package protocol

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// fakePool 模拟真实号池的取号语义：每次 Acquire 给出**不同**的账号，
// 并记录「第几次尝试拿到了哪个号」。
//
// 早期版本让 fake 每次现编一个 "acct-N" 再断言「没有重复」—— 那是自证，
// 无论实现是否正确都会通过。这里改成轮转固定账号集，
// 重复取到同一个号才会暴露实现问题。
type fakePool struct {
	mu       sync.Mutex
	accounts []string
	cursor   int
	taken    []string // 每次 Acquire 的顺序记录
	// generator 决定 Acquire 返回的 generator。nil 时返回空壳。
	//
	// 空壳只能配 Generate 注入使用：一旦有人绕过 generateWithRetry 直接调用
	// generator.Generate，空 Upstream 会 panic。需要「绕过也必须干净失败」
	// 的用例（并行路径）要显式给一个会返回错误的上游。
	generator func() *ImageGenerator

	released int32
}

func newFakePool(names ...string) *fakePool {
	return &fakePool{accounts: names}
}

func (f *fakePool) Acquire(ctx context.Context, index int) (*ImageGenerator, func(), error) {
	f.mu.Lock()
	token := f.accounts[f.cursor%len(f.accounts)]
	f.cursor++
	f.taken = append(f.taken, token)
	f.mu.Unlock()
	if token == "" {
		return nil, nil, errors.New("no available image quota")
	}
	generator := &ImageGenerator{}
	if f.generator != nil {
		generator = f.generator()
	}
	return generator, func() { atomic.AddInt32(&f.released, 1) }, nil
}

func (f *fakePool) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.taken)
}

// errGen 返回固定错误序列的 Generate 注入。
func errGen(fn func(attempt int) ([]ImageOutput, error)) (func(context.Context, *ImageGenerator, ImageRequest) ([]ImageOutput, error), func() int) {
	var counter int32
	return func(ctx context.Context, gen *ImageGenerator, req ImageRequest) ([]ImageOutput, error) {
		attempt := int(atomic.AddInt32(&counter, 1))
		return fn(attempt)
	}, func() int { return int(atomic.LoadInt32(&counter)) }
}

// 轮询超时必须换号重试，最多 4 次（Python MAX_POLL_TIMEOUT_RETRIES）。
//
// 回归点：Go 一次失败就返回 502，而 Python 会换账号重试 4 次。
func TestPoolRetriesPollTimeoutWithDifferentAccount(t *testing.T) {
	fake := newFakePool("acct-1", "acct-2", "acct-3")
	generate, attempts := errGen(func(int) ([]ImageOutput, error) {
		return nil, &ImageGenerationError{
			Message:    "image poll timed out",
			StatusCode: 504,
			Code:       "image_poll_timeout",
		}
	})
	pool := &ImagePool{Acquire: fake.Acquire, Generate: generate}

	if _, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1}); err == nil {
		t.Fatal("all attempts fail, want error")
	}
	if got := attempts(); got != 5 {
		t.Errorf("generate attempts = %d, want 5 (1 initial + 4 poll-timeout retries)", got)
	}
	// 每次重试都必须重新 Acquire（换号的必要条件）。
	// fake 在固定账号集上轮转，所以「相邻两次拿到不同的号」等价于
	// 「每次重试都重新取号」—— 原地复用 generator 不会推进游标。
	fake.mu.Lock()
	taken := append([]string{}, fake.taken...)
	fake.mu.Unlock()
	if len(taken) != 5 {
		t.Fatalf("taken = %v, want 5 acquisitions (one per attempt)", taken)
	}
	for i := 1; i < len(taken); i++ {
		if taken[i] == taken[i-1] {
			t.Errorf("attempt %d did not re-acquire (taken=%v)", i+1, taken)
		}
	}
}

// 并行分支（image_parallel_generation 默认 true）必须走同一套重试。
//
// 回归点：runParallel 里直接 Acquire+Generate，默认配置下四类预算全部失效。
func TestPoolParallelUsesRetryBudgets(t *testing.T) {
	fake := newFakePool("acct-1", "acct-2", "acct-3", "acct-4")
	// 给一个「真实调用会失败但不 panic」的上游：若实现绕过重试直接调用
	// generator.Generate，这里会得到一个未分类错误（尝试 1 次），
	// 与重试路径的 5 次形成对照，而不是把测试炸成 panic。
	fake.generator = func() *ImageGenerator {
		return &ImageGenerator{
			Upstream: &delayedImageFake{
				inner:      &imageFake{downloadBytes: []byte("PNG"), urls: []string{"u"}},
				shouldFail: true,
			},
			Config: fastGenConfig(),
		}
	}
	generate, attempts := errGen(func(int) ([]ImageOutput, error) {
		return nil, &ImageGenerationError{
			Message:    "image poll timed out",
			StatusCode: 504,
			Code:       "image_poll_timeout",
		}
	})
	pool := &ImagePool{Parallel: true, Acquire: fake.Acquire, Generate: generate}

	res, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 2})
	if err == nil {
		t.Fatal("all indexes fail, want error")
	}
	if len(res.Failures) != 2 {
		t.Errorf("failures = %v, want both indexes", res.Failures)
	}
	// 两张图各 1 初始 + 4 重试 = 10。若走旧路径则是 2（零重试）。
	if got := attempts(); got != 10 {
		t.Errorf("parallel generate attempts = %d, want 10 (2 indexes × (1+4))", got)
	}
}

// 并行分支的成功路径不能被重试逻辑破坏。
func TestPoolParallelSucceedsWithoutRetry(t *testing.T) {
	fake := newFakePool("acct-1", "acct-2")
	pool := &ImagePool{
		Parallel: true,
		Acquire:  fake.Acquire,
		Generate: func(ctx context.Context, gen *ImageGenerator, req ImageRequest) ([]ImageOutput, error) {
			return []ImageOutput{{Kind: ImageKindResult, Data: []map[string]any{{"url": "u"}}}}, nil
		},
	}
	res, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Outputs) != 2 {
		t.Errorf("outputs = %d, want 2", len(res.Outputs))
	}
	if got := fake.attemptCount(); got != 2 {
		t.Errorf("acquires = %d, want 2 (no retry on success)", got)
	}
}

// 模型返回文本而非图片：重试 3 次后返回 upstream_text_reply（Python 同）。
func TestPoolExhaustsTextReplyBudget(t *testing.T) {
	fake := newFakePool("acct-1", "acct-2")
	generate, attempts := errGen(func(int) ([]ImageOutput, error) {
		return nil, &ImageGenerationError{
			Message:    "the model returned text instead of an image",
			StatusCode: 502,
			Code:       "text_reply",
		}
	})
	pool := &ImagePool{Acquire: fake.Acquire, Generate: generate}

	_, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1})
	if err == nil {
		t.Fatal("want error after exhausting text-reply budget")
	}
	var genErr *ImageGenerationError
	if !errors.As(err, &genErr) {
		t.Fatalf("error type = %T, want *ImageGenerationError", err)
	}
	if genErr.Code != "upstream_text_reply" {
		t.Errorf("error code = %q, want upstream_text_reply", genErr.Code)
	}
	// 文案逐字对齐 Python。
	const want = "Image generation failed: the upstream model returned a text description " +
		"instead of generating an image. Please try again later."
	if genErr.Message != want {
		t.Errorf("message = %q, want %q", genErr.Message, want)
	}
	if got := attempts(); got != 4 {
		t.Errorf("attempts = %d, want 4 (1 initial + 3 text-reply retries)", got)
	}
}

// 真实 Generate 对「文本回复」不抛错，而是产出 message output + nil error。
// 这种返回也必须进 text-reply 预算，否则该预算永远不会触发。
func TestPoolRetriesMessageOnlyTextReply(t *testing.T) {
	fake := newFakePool("acct-1", "acct-2")
	generate, attempts := errGen(func(int) ([]ImageOutput, error) {
		return []ImageOutput{{
			Kind:           ImageKindMessage,
			Text:           `{"referenced_image_ids":["file-1"]}`,
			AccountEmail:   "text-reply@example.com",
			ConversationID: "conv-text-reply",
		}}, nil
	})
	pool := &ImagePool{Acquire: fake.Acquire, Generate: generate}

	_, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1})
	if err == nil {
		t.Fatal("text-only outputs must exhaust the text-reply budget, got nil error")
	}
	var genErr *ImageGenerationError
	if !errors.As(err, &genErr) || genErr.Code != "upstream_text_reply" {
		t.Fatalf("error = %v, want code upstream_text_reply", err)
	}
	// 现场必须带出来：任务详情要 account_email，resume-poll 要 conversation_id。
	if genErr.AccountEmail != "text-reply@example.com" {
		t.Errorf("account_email = %q, want the last attempt's account", genErr.AccountEmail)
	}
	if genErr.ConversationID != "conv-text-reply" {
		t.Errorf("conversation_id = %q, want the last attempt's conversation", genErr.ConversationID)
	}
	if got := attempts(); got != 4 {
		t.Errorf("attempts = %d, want 4 (1 initial + 3 text-reply retries)", got)
	}
}

// 内容政策拒绝也是 message output，但 Python **不**重试它。
// 放宽成「全部 message 即 text-reply」会白烧 4 次额度并重复触犯上游。
func TestPoolDoesNotRetryContentPolicyMessage(t *testing.T) {
	fake := newFakePool("acct-1", "acct-2")
	pool := &ImagePool{
		Acquire: fake.Acquire,
		Generate: func(ctx context.Context, gen *ImageGenerator, req ImageRequest) ([]ImageOutput, error) {
			return []ImageOutput{{
				Kind: ImageKindMessage,
				Text: "Image generation was rejected by upstream policy.",
			}}, nil
		},
	}
	res, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1})
	if err != nil {
		t.Fatalf("policy message must be returned as-is, got %v", err)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Kind != ImageKindMessage {
		t.Errorf("outputs = %+v", res.Outputs)
	}
	if got := fake.attemptCount(); got != 1 {
		t.Errorf("acquires = %d, want 1 (policy rejection must not retry)", got)
	}
}

// wrapImageError 把 TLS 报错改写成固定文案，原始标记只留在 Cause 里。
// 分类器必须穿透改写，否则 TLS 预算永远空转。
func TestClassifyRetrySeesWrappedTLSError(t *testing.T) {
	raw := errors.New("curl: (35) TLS connect error: openssl_internal")
	wrapped := wrapImageError(raw, "gpt-image-2", "conv-1")

	var genErr *ImageGenerationError
	if !errors.As(wrapped, &genErr) {
		t.Fatalf("wrapped type = %T", wrapped)
	}
	if IsTLSConnectionError(genErr.Message) {
		t.Fatalf("precondition broken: message still carries curl marker (%q)", genErr.Message)
	}
	if got := classifyRetry(wrapped); got != retryTLS {
		t.Errorf("classifyRetry(wrapped TLS) = %v, want retryTLS", got)
	}
	if got := classifyRetry(raw); got != retryTLS {
		t.Errorf("classifyRetry(raw TLS) = %v, want retryTLS", got)
	}
}

// 连接超时（curl 28）改写后同样要能分类。
func TestClassifyRetrySeesWrappedConnTimeout(t *testing.T) {
	raw := errors.New("curl: (28) Operation timed out after 30001 milliseconds")
	wrapped := wrapImageError(raw, "gpt-image-2", "conv-1")
	if got := classifyRetry(wrapped); got != retryConnTimeout {
		t.Errorf("classifyRetry(wrapped conn-timeout) = %v, want retryConnTimeout", got)
	}
}

// TLS 错误在池里必须真的触发预算（端到端，不只是分类器单测）。
func TestPoolRetriesWrappedTLSError(t *testing.T) {
	fake := newFakePool("acct-1", "acct-2")
	generate, attempts := errGen(func(int) ([]ImageOutput, error) {
		return nil, wrapImageError(errors.New("curl: (35) TLS connect error"), "gpt-image-2", "c")
	})
	pool := &ImagePool{Acquire: fake.Acquire, Generate: generate}

	if _, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1}); err == nil {
		t.Fatal("want error")
	}
	if got := attempts(); got != 4 {
		t.Errorf("attempts = %d, want 4 (1 initial + 3 TLS retries)", got)
	}
}

// 未识别错误快速失败，不能进无限循环。
func TestPoolFailsFastOnUnknownError(t *testing.T) {
	fake := newFakePool("acct-1", "acct-2")
	pool := &ImagePool{
		Acquire: fake.Acquire,
		Generate: func(ctx context.Context, gen *ImageGenerator, req ImageRequest) ([]ImageOutput, error) {
			return nil, errors.New("unsupported image model")
		},
	}
	_, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1})
	if err == nil {
		t.Fatal("want error")
	}
	if got := fake.attemptCount(); got != 1 {
		t.Errorf("acquires = %d, want 1 (unclassified errors must not retry)", got)
	}
}

// 成功一次即停止重试。
func TestPoolStopsRetryingOnSuccess(t *testing.T) {
	fake := newFakePool("acct-1", "acct-2")
	pool := &ImagePool{
		Acquire: fake.Acquire,
		Generate: func(ctx context.Context, gen *ImageGenerator, req ImageRequest) ([]ImageOutput, error) {
			if fake.attemptCount() < 2 {
				return nil, &ImageGenerationError{Code: "image_poll_timeout", StatusCode: 504}
			}
			return []ImageOutput{{Kind: ImageKindResult}}, nil
		},
	}
	result, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1})
	if err != nil {
		t.Fatalf("want success after retry, got %v", err)
	}
	if len(result.Outputs) != 1 {
		t.Errorf("outputs = %d, want 1", len(result.Outputs))
	}
	if got := fake.attemptCount(); got != 2 {
		t.Errorf("acquires = %d, want 2 (stop after success)", got)
	}
}

// Generate panic 时槽位必须释放。
//
// 回归点：改成重试循环后 release() 变成裸调用，panic 会跳过它，
// 账号永久留在 inflight（旧 runSingle 用 defer release()）。
func TestPoolReleasesSlotOnPanic(t *testing.T) {
	fake := newFakePool("acct-1")
	pool := &ImagePool{
		Acquire: fake.Acquire,
		Generate: func(ctx context.Context, gen *ImageGenerator, req ImageRequest) ([]ImageOutput, error) {
			panic("boom in generate")
		},
	}

	// 契约变了：panic 不再穿透给调用方，而是转成 error。
	//
	// 为什么改：ImagePool.Run 的三个分支（n=1 / 串行 / 并行）现在统一 recover。
	// 之前只有并行分支有 recover，而 n=1 是 chat 与所有异步任务的默认路径 ——
	// 同步调用时 panic 会穿到裸 `go` 起的调用方，整进程退出。
	//
	// 槽位释放仍然必须成立（下面的断言），那是本测试的真正目的。
	if _, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1}); err == nil {
		t.Error("Generate panic 应转成 error，实际返回 nil")
	}

	if got := atomic.LoadInt32(&fake.released); got != 1 {
		t.Errorf("released = %d, want 1 (slot leaked when Generate panics)", got)
	}
}

// 每次尝试都必须释放一次，不能多也不能少。
func TestPoolReleasesSlotPerAttempt(t *testing.T) {
	fake := newFakePool("acct-1", "acct-2")
	pool := &ImagePool{
		Acquire: fake.Acquire,
		Generate: func(ctx context.Context, gen *ImageGenerator, req ImageRequest) ([]ImageOutput, error) {
			return nil, &ImageGenerationError{Code: "image_poll_timeout", StatusCode: 504}
		},
	}
	if _, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1}); err == nil {
		t.Fatal("want error")
	}
	acquires := fake.attemptCount()
	if got := int(atomic.LoadInt32(&fake.released)); got != acquires {
		t.Errorf("released = %d, acquires = %d (must pair)", got, acquires)
	}
}

func TestItoaTest(t *testing.T) {
	for _, n := range []int{0, 1, 7, 42, 1234} {
		if got, want := itoaTest(n), fmt.Sprintf("%d", n); got != want {
			t.Errorf("itoaTest(%d) = %q, want %q", n, got, want)
		}
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
