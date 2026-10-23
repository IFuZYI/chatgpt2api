package protocol

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// 轮询超时必须换号重试，最多 4 次（Python MAX_POLL_TIMEOUT_RETRIES）。
//
// 回归点：Go 一次失败就返回 502，而 Python 会换账号重试 4 次。
// 后果：某个账号慢/限流时，这张图必失败（Python 会自愈）。
func TestPoolRetriesPollTimeoutWithDifferentAccount(t *testing.T) {
	acquired := []string{}
	var mu sync.Mutex

	pool := &ImagePool{
		Acquire: func(ctx context.Context, index int) (*ImageGenerator, func(), error) {
			mu.Lock()
			acquired = append(acquired, "acct-"+itoaTest(len(acquired)+1))
			mu.Unlock()
			return &ImageGenerator{}, func() {}, nil
		},
		Generate: func(ctx context.Context, gen *ImageGenerator, req ImageRequest) ([]ImageOutput, error) {
			return nil, &ImageGenerationError{
				Message:    "image poll timed out",
				StatusCode: 504,
				Code:       "image_poll_timeout",
			}
		},
	}
	_, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1})

	if err == nil {
		t.Fatal("all attempts fail, want error")
	}
	mu.Lock()
	attempts := len(acquired)
	mu.Unlock()
	// 首次 + 4 次重试 = 5
	if attempts != 5 {
		t.Errorf("acquired %d accounts, want 5 (1 initial + 4 poll-timeout retries)", attempts)
	}
	// 必须换号：不能重复用同一个账号
	seen := map[string]bool{}
	for _, token := range acquired {
		if seen[token] {
			t.Errorf("account %s acquired twice — retry must switch accounts", token)
		}
		seen[token] = true
	}
}

// 模型返回文本而非图片：重试 3 次后返回 upstream_text_reply（Python 同）。
func TestPoolExhaustsTextReplyBudget(t *testing.T) {
	attempts := 0
	pool := &ImagePool{
		Acquire: func(ctx context.Context, index int) (*ImageGenerator, func(), error) {
			attempts++
			return &ImageGenerator{}, func() {}, nil
		},
		Generate: func(ctx context.Context, gen *ImageGenerator, req ImageRequest) ([]ImageOutput, error) {
			return nil, &ImageGenerationError{
				Message:    "the model returned text instead of an image",
				StatusCode: 502,
				Code:       "text_reply",
			}
		},
	}
	_, err := pool.Run(context.Background(), ImageRequest{Prompt: "x", Model: "gpt-image-2", N: 1})

	if err == nil {
		t.Fatal("want error after exhausting text-reply budget")
	}
	var genErr *ImageGenerationError
	if errors.As(err, &genErr) && genErr.Code != "upstream_text_reply" {
		t.Errorf("error code = %q, want upstream_text_reply", genErr.Code)
	}
	// 首次 + 3 次重试 = 4
	if attempts != 4 {
		t.Errorf("attempts = %d, want 4 (1 initial + 3 text-reply retries)", attempts)
	}
}

// 成功一次即停止重试。
func TestPoolStopsRetryingOnSuccess(t *testing.T) {
	attempts := 0
	pool := &ImagePool{
		Acquire: func(ctx context.Context, index int) (*ImageGenerator, func(), error) {
			attempts++
			return &ImageGenerator{}, func() {}, nil
		},
		Generate: func(ctx context.Context, gen *ImageGenerator, req ImageRequest) ([]ImageOutput, error) {
			if attempts < 2 {
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
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (stop after success)", attempts)
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
