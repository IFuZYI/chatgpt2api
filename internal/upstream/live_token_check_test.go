//go:build live

package upstream

import (
	"bufio"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLiveTokenValidity 批量检查 access_token 是否仍有效。
//
// 只在 `go test -tags live` 下运行（依赖外网）。token 从环境变量读，
// 不写进代码 —— 这类凭据不该进仓库。
//
// 用法：
//
//	CHATGPT2API_TEST_TOKENS=/path/to/tokens.txt \
//	  go test -tags live ./internal/upstream/ -run TestLiveTokenValidity -v
//
// 文件格式：每行一个 access_token，# 开头忽略。
func TestLiveTokenValidity(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("CHATGPT2API_TEST_TOKENS"))
	if path == "" {
		t.Skip("set CHATGPT2API_TEST_TOKENS to a newline-separated token file")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Skipf("cannot open %s: %v", path, err)
	}
	defer file.Close()

	var tokens []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tokens = append(tokens, line)
	}
	if len(tokens) == 0 {
		t.Skip("token file is empty")
	}

	var (
		mu      sync.Mutex
		valid   int
		reasons = map[string]int{}
	)
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup

	for _, token := range tokens {
		wg.Add(1)
		sem <- struct{}{}
		go func(tk string) {
			defer wg.Done()
			defer func() { <-sem }()

			client, err := NewClient(ClientConfig{AccessToken: tk, Impersonate: "chrome110"})
			if err != nil {
				mu.Lock()
				reasons["client_build_failed"]++
				mu.Unlock()
				return
			}
			defer client.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()

			if _, err := client.FetchAccountInfo(ctx); err != nil {
				msg := err.Error()
				switch {
				case strings.Contains(msg, "401"):
					msg = "http_401"
				case strings.Contains(msg, "403"):
					msg = "http_403"
				case strings.Contains(msg, "deadline"), strings.Contains(msg, "timeout"):
					msg = "timeout"
				default:
					msg = "other"
				}
				mu.Lock()
				reasons[msg]++
				mu.Unlock()
				return
			}
			mu.Lock()
			valid++
			mu.Unlock()
		}(token)
	}
	wg.Wait()

	invalid := len(tokens) - valid
	t.Logf("total=%d valid=%d invalid=%d reasons=%v", len(tokens), valid, invalid, reasons)
	if invalid > 0 {
		t.Logf("WARNING: %d invalid token(s)", invalid)
	}
}
