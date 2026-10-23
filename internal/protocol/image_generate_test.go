package protocol

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// imageFake 是可编排的图片上游：控制 SSE 流、对话文档、URL 解析与下载。
type imageFake struct {
	fakeImageUpstream
	mu            sync.Mutex
	sseLines      []string
	urls          []string
	downloadBytes []byte
	resolveErr    error
	deleted       []string
	removeCalls   int
}

func (f *imageFake) DoStream(ctx context.Context, path string, payload []byte, req Requirements) (readCloser, error) {
	f.mu.Lock()
	lines := append([]string{}, f.sseLines...)
	f.mu.Unlock()
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, "data: "+line)
	}
	return newStringReadCloser(strings.Join(out, "\n") + "\n"), nil
}

// StreamImage 复用 sseLines，与 DoStream 一致（fake 不需要区分端点）。
func (f *imageFake) StreamImage(ctx context.Context, request ImageStreamRequest) (<-chan string, <-chan error, error) {
	f.mu.Lock()
	lines := append([]string{}, f.sseLines...)
	f.mu.Unlock()

	payloads := make(chan string, len(lines))
	errs := make(chan error, 1)
	for _, line := range lines {
		payloads <- line
	}
	close(payloads)
	close(errs)
	return payloads, errs, nil
}

func (f *imageFake) ResolveImageURLs(ctx context.Context, conversationID string, fileIDs, sedimentIDs []string) ([]string, error) {
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	return f.urls, nil
}

func (f *imageFake) DownloadImage(ctx context.Context, url string) ([]byte, error) {
	return f.downloadBytes, nil
}

func (f *imageFake) DeleteConversation(ctx context.Context, conversationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, conversationID)
	return nil
}

func newImageFake() *imageFake {
	return &imageFake{
		downloadBytes: []byte("PNGDATA"),
		urls:          []string{"https://example.test/img.png"},
	}
}

func fastGenConfig() PollOptions {
	return PollOptions{
		TimeoutSecs:     1,
		IntervalSecs:    0.01,
		InitialWaitSecs: 0,
		SettleEnabled:   true,
		CheckBeforeHit:  false,
		SettleSecs:      0.01,
	}
}

// 正常路径：SSE 带图 → 下载 → result。
func TestGenerateProducesResult(t *testing.T) {
	upstream := newImageFake()
	upstream.sseLines = []string{
		`{"p":"/message/content/parts/0","o":"append","v":"generating"}`,
		`{"conversation_id":"conv-1","type":"server_ste_metadata","metadata":{"turn_use_case":"image gen"}}`,
		`[DONE]`,
	}
	upstream.conversations = []map[string]any{conversationWithImage("img1")}

	generator := &ImageGenerator{
		Upstream: upstream,
		Config:   fastGenConfig(),
		SaveImage: func(payload []byte, baseURL string) (string, error) {
			return "http://h/images/x.png", nil
		},
	}
	outputs, err := generator.Generate(context.Background(), ImageRequest{
		Prompt: "a cat", Model: "gpt-image-2", N: 1, ResponseFormat: "b64_json", BaseURL: "http://h",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var sawProgress, sawResult bool
	for _, output := range outputs {
		switch output.Kind {
		case ImageKindProgress:
			sawProgress = true
		case ImageKindResult:
			sawResult = true
			if len(output.Data) != 1 {
				t.Errorf("result data len = %d", len(output.Data))
			}
		}
	}
	if !sawProgress {
		t.Error("expected at least one progress output")
	}
	if !sawResult {
		t.Fatalf("expected a result output, got %d outputs", len(outputs))
	}
}

// 不支持的模型要立即报错，不发请求。
func TestGenerateRejectsUnsupportedModel(t *testing.T) {
	upstream := newImageFake()
	generator := &ImageGenerator{Upstream: upstream, Config: fastGenConfig()}
	_, err := generator.Generate(context.Background(), ImageRequest{Prompt: "x", Model: "dall-e-3"})
	if err == nil {
		t.Fatal("expected error for unsupported model")
	}
	var generationErr *ImageGenerationError
	if !errors.As(err, &generationErr) {
		t.Fatalf("error type = %T", err)
	}
	if generationErr.StatusCode != 400 {
		t.Errorf("status = %d, want 400", generationErr.StatusCode)
	}
	if !strings.Contains(err.Error(), "gpt-image-2") {
		t.Errorf("error should list supported models: %v", err)
	}
}

// 被 moderation 拦下时返回 message，不抛错（让上层转成友好文案）。
func TestGenerateReturnsMessageOnBlocked(t *testing.T) {
	upstream := newImageFake()
	upstream.sseLines = []string{
		`{"type":"moderation","moderation_response":{"blocked":true},"conversation_id":"conv-b"}`,
		`{"p":"/message/content/parts/0","o":"append","v":"I can't help with that"}`,
		`[DONE]`,
	}
	generator := &ImageGenerator{Upstream: upstream, Config: fastGenConfig()}
	outputs, err := generator.Generate(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2"})
	if err != nil {
		t.Fatalf("blocked should not error: %v", err)
	}
	found := false
	for _, output := range outputs {
		if output.Kind == ImageKindMessage && output.Text != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a message output, got %v", outputs)
	}
}

// 上游只回文本（无图、非图片场景）时，文本作为 message 返回。
func TestGenerateReturnsPlainMessage(t *testing.T) {
	upstream := newImageFake()
	upstream.sseLines = []string{
		`{"p":"/message/content/parts/0","o":"append","v":"I cannot draw that"}`,
		`[DONE]`,
	}
	generator := &ImageGenerator{Upstream: upstream, Config: fastGenConfig()}
	outputs, err := generator.Generate(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, output := range outputs {
		if output.Kind == ImageKindMessage && strings.Contains(output.Text, "cannot draw") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected plain message output, got %v", outputs)
	}
}

// 既没图也没文本时，要给可操作的兜底文案（不是模糊报错）。
func TestGenerateFallbackMessage(t *testing.T) {
	upstream := newImageFake()
	upstream.sseLines = []string{`[DONE]`}
	generator := &ImageGenerator{Upstream: upstream, Config: fastGenConfig()}
	outputs, err := generator.Generate(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(outputs) == 0 {
		t.Fatal("expected at least a fallback message")
	}
	last := outputs[len(outputs)-1]
	if last.Kind != ImageKindMessage || last.Text == "" {
		t.Fatalf("expected fallback message, got %v", last)
	}
}

// 下载失败时要退回 message，而不是崩掉。
func TestGenerateHandlesDownloadFailure(t *testing.T) {
	upstream := newImageFake()
	upstream.sseLines = []string{
		`{"conversation_id":"c","type":"server_ste_metadata","metadata":{"turn_use_case":"image gen"}}`,
		`[DONE]`,
	}
	upstream.conversations = []map[string]any{conversationWithImage("img1")}
	upstream.downloadBytes = nil // 下载返回空
	upstream.urls = []string{"https://example.test/x.png"}

	generator := &ImageGenerator{Upstream: upstream, Config: fastGenConfig()}
	outputs, err := generator.Generate(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2"})
	if err != nil {
		t.Fatalf("download failure should not error: %v", err)
	}
	for _, output := range outputs {
		if output.Kind == ImageKindResult {
			t.Fatal("should not produce result when download fails")
		}
	}
}

// 落盘失败（WebDAV 5xx / 磁盘满）必须让生成失败，不能降级成 message。
//
// 回归点：SaveImage 的错误曾被 FormatImageResult 丢弃，于是 webdav 模式的
// 用户拿到 200 + 空 url，以为图存好了，实际远端与本地都没有。
func TestGenerateFailsWhenSaveImageFails(t *testing.T) {
	upstream := newImageFake()
	upstream.sseLines = []string{
		`{"conversation_id":"conv-s","type":"server_ste_metadata","metadata":{"turn_use_case":"image gen"}}`,
		`[DONE]`,
	}
	upstream.conversations = []map[string]any{conversationWithImage("img1")}

	generator := &ImageGenerator{
		Upstream: upstream,
		Config:   fastGenConfig(),
		SaveImage: func(payload []byte, baseURL string) (string, error) {
			return "", errors.New("WebDAV PUT failed: HTTP 500")
		},
	}
	outputs, err := generator.Generate(context.Background(), ImageRequest{
		Prompt: "a cat", Model: "gpt-image-2", N: 1, ResponseFormat: "b64_json", BaseURL: "http://h",
	})
	if err == nil {
		t.Fatalf("save failure must fail the generation, got outputs=%v", outputs)
	}
	for _, output := range outputs {
		if output.Kind == ImageKindResult {
			t.Fatalf("save failure must not produce a result output: %v", output)
		}
	}
	var generationErr *ImageGenerationError
	if !errors.As(err, &generationErr) {
		t.Fatalf("error type = %T, want *ImageGenerationError", err)
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error should keep the WebDAV status: %v", err)
	}
}

// 尺寸/质量提示要拼进提示词（上游按提示词理解尺寸）。
func TestPromptWithImageHints(t *testing.T) {
	got := promptWithImageHints("a cat", "1024x1024", "high")
	if !strings.Contains(got, "a cat") {
		t.Errorf("prompt lost: %q", got)
	}
	if !strings.Contains(got, "1024x1024") {
		t.Errorf("size hint missing: %q", got)
	}
	if !strings.Contains(got, "high") {
		t.Errorf("quality hint missing: %q", got)
	}
	// 无提示时原样返回
	if got := promptWithImageHints("a cat", nil, ""); got != "a cat" {
		t.Errorf("no hints should return prompt unchanged, got %q", got)
	}
}

// 成功出图后要触发对话清理。
func TestGenerateCleansUpConversation(t *testing.T) {
	upstream := newImageFake()
	upstream.sseLines = []string{
		`{"conversation_id":"conv-x","type":"server_ste_metadata","metadata":{"turn_use_case":"image gen"}}`,
		`[DONE]`,
	}
	upstream.conversations = []map[string]any{conversationWithImage("img1")}

	cleanupCalls := make([]string, 0)
	config := fastGenConfig()
	config.RemoveConversation = func(conversationID string, success bool) {
		cleanupCalls = append(cleanupCalls, conversationID)
	}

	generator := &ImageGenerator{Upstream: upstream, Config: config}
	if _, err := generator.Generate(context.Background(), ImageRequest{
		Prompt: "x", Model: "gpt-image-2", ResponseFormat: "b64_json",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cleanupCalls) == 0 || cleanupCalls[0] != "conv-x" {
		t.Errorf("cleanup not called with conversation id: %v", cleanupCalls)
	}
}

// 轮询超时要包装成 ImageGenerationError 且带 504。
func TestGenerateWrapsPollTimeout(t *testing.T) {
	upstream := newImageFake()
	upstream.sseLines = []string{
		`{"conversation_id":"conv-t","type":"server_ste_metadata","metadata":{"turn_use_case":"image gen"}}`,
		`[DONE]`,
	}
	upstream.conversations = []map[string]any{{}} // 永远没有图

	config := fastGenConfig()
	config.TimeoutSecs = 0.1
	generator := &ImageGenerator{Upstream: upstream, Config: config}

	_, err := generator.Generate(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var generationErr *ImageGenerationError
	if !errors.As(err, &generationErr) {
		t.Fatalf("error type = %T, want ImageGenerationError", err)
	}
	if generationErr.StatusCode != 504 {
		t.Errorf("status = %d, want 504", generationErr.StatusCode)
	}
	if generationErr.Code != "image_poll_timeout" {
		t.Errorf("code = %q", generationErr.Code)
	}
}

var _ = time.Second

// OnResult 只在真的产出图片时调用（决定是否扣额度）。
func TestGenerateOnResultOnlyOnSuccess(t *testing.T) {
	// 成功场景
	successUpstream := newImageFake()
	successUpstream.sseLines = []string{
		`{"conversation_id":"c1","type":"server_ste_metadata","metadata":{"turn_use_case":"image gen"}}`,
		`[DONE]`,
	}
	successUpstream.conversations = []map[string]any{conversationWithImage("img1")}

	successCalls := 0
	generator := &ImageGenerator{
		Upstream: successUpstream,
		Config:   fastGenConfig(),
		OnResult: func() { successCalls++ },
	}
	if _, err := generator.Generate(context.Background(), ImageRequest{
		Prompt: "x", Model: "gpt-image-2", ResponseFormat: "b64_json",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if successCalls != 1 {
		t.Errorf("OnResult calls = %d, want 1 on success", successCalls)
	}

	// 失败场景：只回文本，没有图片
	failUpstream := newImageFake()
	failUpstream.sseLines = []string{
		`{"p":"/message/content/parts/0","o":"append","v":"I cannot do that"}`,
		`[DONE]`,
	}
	failCalls := 0
	failGenerator := &ImageGenerator{
		Upstream: failUpstream,
		Config:   fastGenConfig(),
		OnResult: func() { failCalls++ },
	}
	if _, err := failGenerator.Generate(context.Background(), ImageRequest{
		Prompt: "x", Model: "gpt-image-2", ResponseFormat: "b64_json",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if failCalls != 0 {
		t.Errorf("OnResult calls = %d, want 0 when no image produced", failCalls)
	}
}
