package api_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/IFuZYI/chatgpt2api/internal/service"
)

// TestStreamImageGenerationFailureEmitsErrorEvent 是「流式生图失败被吞」的回归用例。
//
// 回归点：SSE 头一旦发出就改不了状态码。修复前 runErr != nil 时只记日志、
// 直接 return，客户端收到 HTTP 200 + 空的 `data: [DONE]` —— 与「成功但 0 张图」
// 完全无法区分，是生图接口最糟的失败形态。必须在流里补一条 error 事件。
func TestStreamImageGenerationFailureEmitsErrorEvent(t *testing.T) {
	deps := newTestDeps(t)
	deps.Accts.AddAccountItems([]map[string]any{
		{"access_token": "token-1", "email": "img@example.test", "type": "plus",
			"status": service.StatusNormal, "quota": 10},
	}, nil)
	// 工厂直接报错 → pool.Run 拿不到上游 → 必定失败。
	deps.ImageUpstream = func(ctx context.Context, token string) (protocol.ImageUpstream, func(), error) {
		return nil, func() {}, errors.New("upstream exploded")
	}

	router := newTestRouter(t, deps)
	body := `{"model":"gpt-image-2","prompt":"x","n":1,"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	raw, _ := io.ReadAll(rec.Result().Body)
	text := string(raw)

	if !strings.Contains(text, `"error"`) {
		t.Fatalf("流里没有 error 事件，客户端会把失败当成功。实际响应:\n%s", text)
	}
	if !strings.Contains(text, "server_error") {
		t.Fatalf("error 事件缺少 type=server_error。实际响应:\n%s", text)
	}
	// 顺序断言：error 必须在 [DONE] **之前**。
	//
	// 标准 SSE 客户端读到 [DONE] 就停止解析，若 error 排在它后面，
	// 客户端看到的仍是干净的空成功 —— 原来的测试只检查「error 存在」，
	// 所以这个顺序回归测不出来。
	doneAt := strings.Index(text, "[DONE]")
	errAt := strings.Index(text, `"error"`)
	if doneAt < 0 {
		t.Fatalf("流里没有 [DONE]。实际响应:\n%s", text)
	}
	if errAt > doneAt {
		t.Fatalf("error 事件在 [DONE] 之后（done@%d err@%d），客户端会当成成功。实际响应:\n%s",
			doneAt, errAt, text)
	}
}
