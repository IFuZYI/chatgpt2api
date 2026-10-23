package protocol

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeImageUpstream 按脚本回放对话文档，用于测轮询状态机。
type fakeImageUpstream struct {
	streamStub
	mu sync.Mutex
	// conversations 按调用次序返回；用尽后重复最后一个。
	conversations []map[string]any
	callCount     int
	tasks         []map[string]any
	getErr        []error
	now           func() time.Time
}

func (f *fakeImageUpstream) ChatRequirements(ctx context.Context) (Requirements, error) {
	return Requirements{Token: "t"}, nil
}

func (f *fakeImageUpstream) DoStream(ctx context.Context, path string, payload []byte, req Requirements) (readCloser, error) {
	return newStringReadCloser(""), nil
}

func (f *fakeImageUpstream) Authenticated() bool { return true }

func (f *fakeImageUpstream) UploadImage(payload []byte, filename, mime string) (map[string]any, error) {
	return map[string]any{"file_id": "f"}, nil
}

func (f *fakeImageUpstream) GetConversation(ctx context.Context, conversationID string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.callCount
	f.callCount++
	if index < len(f.getErr) && f.getErr[index] != nil {
		return nil, f.getErr[index]
	}
	if len(f.conversations) == 0 {
		return map[string]any{}, nil
	}
	if index >= len(f.conversations) {
		index = len(f.conversations) - 1
	}
	return f.conversations[index], nil
}

func (f *fakeImageUpstream) QueryTasks(ctx context.Context, conversationID string) ([]map[string]any, error) {
	return f.tasks, nil
}

func (f *fakeImageUpstream) ResolveImageURLs(ctx context.Context, conversationID string, fileIDs, sedimentIDs []string) ([]string, error) {
	return nil, nil
}

func (f *fakeImageUpstream) DownloadImage(ctx context.Context, url string) ([]byte, error) {
	return nil, nil
}

func (f *fakeImageUpstream) FindConversationByPrompt(ctx context.Context, prompt string, since time.Time) (string, error) {
	return "", nil
}

func (f *fakeImageUpstream) DeleteConversation(ctx context.Context, conversationID string) error {
	return nil
}

// conversationWithImage 构造一个含图片工具记录的对话文档。
func conversationWithImage(fileID string) map[string]any {
	return map[string]any{
		"mapping": map[string]any{
			"n1": map[string]any{
				"message": map[string]any{
					"author":      map[string]any{"role": "tool"},
					"create_time": float64(1),
					"content": map[string]any{
						"parts": []any{
							map[string]any{"asset_pointer": "file-service://" + fileID},
						},
					},
					"metadata": map[string]any{"async_task_type": "image_gen"},
				},
			},
		},
	}
}

// fastPollOptions 把等待都压到毫秒级，避免测试变慢。
func fastPollOptions() PollOptions {
	return PollOptions{
		TimeoutSecs:     2,
		IntervalSecs:    0.01,
		InitialWaitSecs: 0,
		SettleEnabled:   true,
		CheckBeforeHit:  true,
		SettleSecs:      0.01,
	}
}

// 找到 id 后要连续两次看到同一组才算落定（check_before_hit 语义）。
func TestPollImageResultsRequiresSettle(t *testing.T) {
	upstream := &fakeImageUpstream{
		conversations: []map[string]any{conversationWithImage("img1")},
	}
	result, err := PollImageResults(context.Background(), upstream, "conv-1", fastPollOptions(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.FileIDs) != 1 || result.FileIDs[0] != "img1" {
		t.Fatalf("file_ids = %v", result.FileIDs)
	}
	// 至少两次调用：第一次发现，第二次确认
	if upstream.callCount < 2 {
		t.Errorf("callCount = %d, want >= 2 (settle requires a second look)", upstream.callCount)
	}
}

// 关闭 check_before_hit 时首次发现即返回。
func TestPollImageResultsNoCheckBeforeHit(t *testing.T) {
	opts := fastPollOptions()
	opts.CheckBeforeHit = false
	upstream := &fakeImageUpstream{
		conversations: []map[string]any{conversationWithImage("img1")},
	}
	if _, err := PollImageResults(context.Background(), upstream, "c", opts, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if upstream.callCount != 1 {
		t.Errorf("callCount = %d, want 1", upstream.callCount)
	}
}

// 已有初始 id 且 settle 关闭时直接返回，不发起请求。
func TestPollImageResultsInitialIDsNoSettle(t *testing.T) {
	opts := fastPollOptions()
	opts.SettleEnabled = false
	opts.CheckBeforeHit = false
	upstream := &fakeImageUpstream{}
	result, err := PollImageResults(context.Background(), upstream, "c", opts, []string{"pre"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.FileIDs) != 1 || result.FileIDs[0] != "pre" {
		t.Fatalf("file_ids = %v", result.FileIDs)
	}
}

// 一直没 id 时超时，错误类型必须是 ImagePollTimeoutError。
func TestPollImageResultsTimeout(t *testing.T) {
	opts := PollOptions{
		TimeoutSecs:     0.15,
		IntervalSecs:    0.02,
		InitialWaitSecs: 0,
		SettleEnabled:   true,
		CheckBeforeHit:  true,
		SettleSecs:      0.01,
	}
	upstream := &fakeImageUpstream{conversations: []map[string]any{{}}}
	_, err := PollImageResults(context.Background(), upstream, "c", opts, nil, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var timeoutErr *ImagePollTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error type = %T, want ImagePollTimeoutError", err)
	}
	if !strings.Contains(err.Error(), "image_poll_timeout_secs") {
		t.Errorf("timeout message should mention the config key: %q", err.Error())
	}
}

// 对话里出现内容政策违规时立即失败，不等超时。
func TestPollImageResultsContentPolicyFailsFast(t *testing.T) {
	opts := PollOptions{
		TimeoutSecs:     5,
		IntervalSecs:    0.01,
		InitialWaitSecs: 0,
		SettleEnabled:   true,
		CheckBeforeHit:  true,
		SettleSecs:      0.01,
	}
	policyConversation := map[string]any{
		"mapping": map[string]any{
			"n1": map[string]any{
				"message": map[string]any{
					"author":  map[string]any{"role": "assistant"},
					"content": map[string]any{"parts": []any{"内容政策违规，不能生成"}},
				},
			},
		},
	}
	upstream := &fakeImageUpstream{conversations: []map[string]any{policyConversation}}

	started := time.Now()
	_, err := PollImageResults(context.Background(), upstream, "c", opts, nil, nil)
	elapsed := time.Since(started)

	var policyErr *ImageContentPolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("error type = %T, want ImageContentPolicyError", err)
	}
	// 应该远早于 5s 超时
	if elapsed > 1*time.Second {
		t.Errorf("took %v, should fail fast on content policy", elapsed)
	}
}

// 上游 5xx 要退避重试而不是立即失败。
func TestPollImageResultsRetriesOn5xx(t *testing.T) {
	opts := PollOptions{
		TimeoutSecs:     3,
		IntervalSecs:    0.01,
		InitialWaitSecs: 0,
		SettleEnabled:   true,
		CheckBeforeHit:  false,
		SettleSecs:      0.01,
	}
	upstream := &fakeImageUpstream{
		conversations: []map[string]any{
			conversationWithImage("img1"),
		},
		getErr: []error{
			NewHTTPStatusError(503, nil, "service unavailable"),
			nil, // 第二次成功
		},
	}
	result, err := PollImageResults(context.Background(), upstream, "c", opts, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.FileIDs) != 1 {
		t.Fatalf("file_ids = %v, want img1 after retry", result.FileIDs)
	}
}

// ctx 取消要能打断轮询（否则客户端断连后 goroutine 泄漏）。
func TestPollImageResultsRespectsContextCancel(t *testing.T) {
	opts := PollOptions{
		TimeoutSecs:     30,
		IntervalSecs:    0.05,
		InitialWaitSecs: 0,
		SettleEnabled:   true,
		CheckBeforeHit:  true,
		SettleSecs:      0.01,
	}
	upstream := &fakeImageUpstream{conversations: []map[string]any{{}}}
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	_, err := PollImageResults(ctx, upstream, "c", opts, nil, nil)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected error after cancel")
	}
	if elapsed > 2*time.Second {
		t.Errorf("cancel took %v, should interrupt promptly", elapsed)
	}
}

// tasks 里的错误只记录不中断（上游会把工具参数标成 is_error）。
func TestPollImageResultsTaskErrorDoesNotBlock(t *testing.T) {
	opts := fastPollOptions()
	opts.CheckBeforeHit = false
	upstream := &fakeImageUpstream{
		conversations: []map[string]any{conversationWithImage("img1")},
		tasks: []map[string]any{
			{
				"image_gen_message": map[string]any{
					"metadata": map[string]any{"is_error": true},
					"content":  map[string]any{"content_type": "text", "parts": []any{"tool params error"}},
					"author":   map[string]any{"role": "assistant"},
				},
			},
		},
	}
	result, err := PollImageResults(context.Background(), upstream, "c", opts, nil, nil)
	if err != nil {
		t.Fatalf("task error should not abort polling: %v", err)
	}
	if len(result.FileIDs) != 1 {
		t.Fatalf("file_ids = %v", result.FileIDs)
	}
	if result.TaskError != "tool params error" {
		t.Errorf("task error should be recorded, got %q", result.TaskError)
	}
}
