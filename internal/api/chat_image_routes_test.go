package api_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/api"
	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/basketikun/chatgpt2api/internal/service"
)

// chatImageUpstream 是 chat 图片链路的测试替身：
// 记录送进上游的请求，回放一段带图结果的 SSE + 对话文档 + 下载字节。
// 全程不碰网络。
type chatImageUpstream struct {
	mu       sync.Mutex
	requests []protocol.ImageStreamRequest
	prompt   string
	images   [][3]string
	// sseLines 是回放给 StreamImage 的事件序列。
	sseLines []string
	// conversation 覆盖 GetConversation 的返回（nil 时用带图的默认文档）。
	conversation map[string]any
	// imageBytes 是 DownloadImage 返回的图片字节（默认一张 1x1 PNG）。
	imageBytes []byte
	// failStream 非 nil 时 StreamImage 直接返回该错误。
	failStream error
	// message 非空时在 [DONE] 之前回放一条文本增量（模拟上游拒图/文本回复）。
	message string
}

func (u *chatImageUpstream) ChatRequirements(ctx context.Context) (protocol.Requirements, error) {
	return protocol.Requirements{Token: "t"}, nil
}

func (u *chatImageUpstream) DoStream(ctx context.Context, path string, payload []byte, req protocol.Requirements) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (u *chatImageUpstream) Authenticated() bool { return true }

func (u *chatImageUpstream) UploadImage(payload []byte, filename, mime string) (map[string]any, error) {
	return map[string]any{"file_id": "file-1"}, nil
}

func (u *chatImageUpstream) GetConversation(ctx context.Context, conversationID string) (map[string]any, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.conversation != nil {
		return u.conversation, nil
	}
	return chatImageConversation(), nil
}

func (u *chatImageUpstream) QueryTasks(ctx context.Context, conversationID string) ([]map[string]any, error) {
	return nil, nil
}

func (u *chatImageUpstream) ResolveImageURLs(ctx context.Context, conversationID string, fileIDs, sedimentIDs []string) ([]string, error) {
	return []string{"https://upstream.test/image.png"}, nil
}

func (u *chatImageUpstream) DownloadImage(ctx context.Context, url string) ([]byte, error) {
	return u.imageBytes, nil
}

func (u *chatImageUpstream) FindConversationByPrompt(ctx context.Context, prompt string, since time.Time) (string, error) {
	return "", nil
}

func (u *chatImageUpstream) DeleteConversation(ctx context.Context, conversationID string) error {
	return nil
}

func (u *chatImageUpstream) StreamImage(ctx context.Context, request protocol.ImageStreamRequest) (<-chan string, <-chan error, error) {
	u.mu.Lock()
	u.requests = append(u.requests, request)
	u.prompt = request.Prompt
	u.images = append([][3]string{}, request.Images...)
	lines := append([]string{}, u.sseLines...)
	message := u.message
	failStream := u.failStream
	u.mu.Unlock()

	if failStream != nil {
		return nil, nil, failStream
	}
	// message 必须插在 [DONE] 之前：消费端读到 [DONE] 就 break，之后再发的事件
	// 全部丢弃（实测踩到：message 追加在 [DONE] 后面 → 上游文本永远到不了）。
	lines = append(linesWithoutDone(lines), messageLines(message)...)
	lines = append(lines, "[DONE]")

	payloads := make(chan string, len(lines)+1)
	errs := make(chan error, 1)
	for _, line := range lines {
		payloads <- line
	}
	close(payloads)
	close(errs)
	return payloads, errs, nil
}

// linesWithoutDone 去掉事件序列里的 [DONE] 终止标记。
func linesWithoutDone(lines []string) []string {
	out := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		if line == "[DONE]" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// messageLines 把一段文本回放成 conversation.delta 事件（空串时无事件）。
func messageLines(message string) []string {
	if message == "" {
		return nil
	}
	return []string{`{"p":"/message/content/parts/0","o":"append","v":` + mustJSONString(message) + `}`}
}

func (u *chatImageUpstream) snapshot() ([]protocol.ImageStreamRequest, [][3]string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]protocol.ImageStreamRequest{}, u.requests...), append([][3]string{}, u.images...)
}

// chatImageConversation 构造一个含图片工具记录的对话文档（轮询据此拿到 file_id）。
func chatImageConversation() map[string]any {
	return map[string]any{
		"mapping": map[string]any{
			"n1": map[string]any{
				"message": map[string]any{
					"author":      map[string]any{"role": "tool"},
					"create_time": float64(1),
					"content": map[string]any{
						"parts": []any{
							map[string]any{"asset_pointer": "file-service://file-1"},
						},
					},
					"metadata": map[string]any{"async_task_type": "image_gen"},
				},
			},
		},
	}
}

func mustJSONString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

// newChatImageServer 建一个注入了假图片上游的测试服务器（带一个可用账号）。
//
// configure 在建路由之前执行，用于注入内容过滤器等依赖（必须在 NewRouter 前，
// 否则 handler 拿到的是旧 deps 值）。
func newChatImageServer(t *testing.T, configure ...func(*api.Deps)) (*httptest.Server, *chatImageUpstream, *api.Deps) {
	t.Helper()
	deps := newTestDeps(t)
	deps.Accts.AddAccountItems([]map[string]any{
		{"access_token": "token-1", "email": "img@example.test", "type": "plus",
			"status": service.StatusNormal, "quota": 10},
	}, nil)

	upstream := &chatImageUpstream{
		imageBytes: decodeChatTestPNG(t),
		sseLines: []string{
			`{"conversation_id":"conv-1","type":"server_ste_metadata","metadata":{"turn_use_case":"image gen"}}`,
			`[DONE]`,
		},
	}
	deps.ImageUpstream = func(ctx context.Context, accessToken string) (protocol.ImageUpstream, func(), error) {
		return upstream, func() {}, nil
	}
	deps.RegisterImageTaskRunners()
	for _, apply := range configure {
		apply(deps)
	}
	// 轮询等待压到毫秒级：默认 10s 首等 + 2s 二次确认会让每条用例跑十几秒。
	if _, err := deps.Config.Update(map[string]any{
		"image_poll_timeout_secs":        2,
		"image_poll_interval_secs":       0.5,
		"image_poll_initial_wait_secs":   0,
		"image_settle_secs":              0.5,
		"image_check_before_hit_enabled": false,
	}); err != nil {
		t.Fatalf("configure poll timings: %v", err)
	}

	server := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(server.Close)
	return server, upstream, deps
}

// decodeChatTestPNG 返回一张 1x1 PNG 的字节。
func decodeChatTestPNG(t *testing.T) []byte {
	t.Helper()
	payload, err := base64.StdEncoding.DecodeString(chatTestPNGBase64)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	return payload
}

const chatTestPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// 非流式：choices[0].message.content 必须是 markdown 图片。
//
// 回归点：这条路径此前固定返回 501，客户端拿不到任何图。
func TestChatImageNonStreamReturnsMarkdownContent(t *testing.T) {
	server, upstream, _ := newChatImageServer(t)

	status, payload := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model": "gpt-image-2",
		"messages": []any{
			map[string]any{"role": "user", "content": "画一只猫"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, payload)
	}
	if body["object"] != "chat.completion" {
		t.Errorf("object = %#v", body["object"])
	}

	choices, _ := body["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %d", len(choices))
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	content, _ := message["content"].(string)
	if !strings.HasPrefix(content, "![image_1](data:image/png;base64,") {
		t.Errorf("content = %q, want markdown image", content)
	}
	if !strings.Contains(content, chatTestPNGBase64) {
		t.Error("content must inline the base64 payload")
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %#v", choice["finish_reason"])
	}

	// 送上游的 prompt 是抽取后的文本（含尺寸/质量提示，由 protocol 层拼）。
	requests, _ := upstream.snapshot()
	if len(requests) != 1 {
		t.Fatalf("stream requests = %d, want 1", len(requests))
	}
	if !strings.HasPrefix(requests[0].Prompt, "画一只猫") {
		t.Errorf("upstream prompt = %q", requests[0].Prompt)
	}
}

// 流式：首 chunk 带 role，后续不带，结尾 finish_reason=stop + [DONE]。
func TestChatImageStreamChunkShape(t *testing.T) {
	server, _, _ := newChatImageServer(t)

	status, raw := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model":  "gpt-image-2",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "画一只猫"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}

	events := parseChatSSE(t, string(raw))
	if len(events) == 0 {
		t.Fatalf("no SSE events: %s", raw)
	}
	if last := events[len(events)-1]; last != "[DONE]" {
		t.Errorf("last event = %q, want [DONE]", last)
	}

	chunks := decodeChatChunks(t, events[:len(events)-1])
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want at least 2 (content + stop)", len(chunks))
	}

	// 首块带 role。
	if delta := chatChunkDelta(t, chunks[0]); delta["role"] != "assistant" {
		t.Errorf("first chunk role = %#v, want assistant", delta["role"])
	}
	// 后续块不带 role。
	for index := 1; index < len(chunks)-1; index++ {
		if _, ok := chatChunkDelta(t, chunks[index])["role"]; ok {
			t.Errorf("chunk %d must not carry role", index)
		}
	}
	// 结尾 finish_reason=stop，delta 为空对象。
	lastChunk := chunks[len(chunks)-1]
	if reason := chatChunkFinishReason(t, lastChunk); reason != "stop" {
		t.Errorf("last chunk finish_reason = %#v, want stop", reason)
	}
	if delta := chatChunkDelta(t, lastChunk); len(delta) != 0 {
		t.Errorf("last chunk delta = %#v, want empty", delta)
	}

	// 所有 chunk 共用同一个 id。
	id, _ := chunks[0]["id"].(string)
	if !strings.HasPrefix(id, "chatcmpl-") {
		t.Errorf("id = %q", id)
	}
	for index, chunk := range chunks {
		if chunk["id"] != id {
			t.Errorf("chunk %d id = %#v, want %q", index, chunk["id"], id)
		}
	}
}

// 流式的完整内容要能拼出 markdown（客户端按 delta 累积）。
func TestChatImageStreamAccumulatesMarkdown(t *testing.T) {
	server, _, _ := newChatImageServer(t)

	status, raw := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model":  "gpt-image-2",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "画一只猫"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}

	var builder strings.Builder
	for _, chunk := range decodeChatChunks(t, parseChatSSE(t, string(raw))[:len(parseChatSSE(t, string(raw)))-1]) {
		if content, ok := chatChunkDelta(t, chunk)["content"].(string); ok {
			builder.WriteString(content)
		}
	}
	if !strings.Contains(builder.String(), "data:image/png;base64,") {
		t.Errorf("accumulated content = %q, want markdown image", builder.String())
	}
	if !strings.Contains(builder.String(), chatTestPNGBase64) {
		t.Errorf("accumulated content missing base64 payload: %q", builder.String())
	}
}

// 空 prompt 报 400（Python 的 chat_image_args 抛 HTTPException(400)）。
func TestChatImageEmptyPromptReturns400(t *testing.T) {
	server, _, _ := newChatImageServer(t)

	status, payload := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model":    "gpt-image-2",
		"messages": []any{map[string]any{"role": "user", "content": "   "}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	if !strings.Contains(string(payload), "prompt is required") {
		t.Errorf("body = %s, want prompt is required", payload)
	}
}

// n 越界报 400（chat 链路用 parse_image_count，不是 pydantic 的 422）。
func TestChatImageRejectsOutOfRangeN(t *testing.T) {
	server, _, _ := newChatImageServer(t)

	for _, n := range []any{float64(5), float64(-1), "abc"} {
		status, payload := postJSON(t, server, "/v1/chat/completions", map[string]any{
			"model":    "gpt-image-2",
			"n":        n,
			"messages": []any{map[string]any{"role": "user", "content": "画一只猫"}},
		})
		if status != http.StatusBadRequest {
			t.Errorf("n=%v status = %d, want 400 (body: %s)", n, status, payload)
		}
	}
}

// n 缺省为 1，且多图请求要把 n 传给上游池（这里只验证 n=2 被接受并跑到上游）。
func TestChatImageAcceptsNWithinRange(t *testing.T) {
	server, upstream, _ := newChatImageServer(t)

	status, payload := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model":    "gpt-image-2",
		"n":        float64(1),
		"messages": []any{map[string]any{"role": "user", "content": "画一只猫"}},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	requests, _ := upstream.snapshot()
	if len(requests) != 1 {
		t.Errorf("stream requests = %d, want 1", len(requests))
	}
}

// modalities 含 image 也走图片链路（即使 model 不是图片模型）。
//
// 断言的是**分流**：请求进了生图链路（拿到「不支持的图片模型」而不是文本链路的
// upstream 错误）。Python 同样会在这里报 unsupported image model
// （is_image_chat_request 只看 modalities，不看 model 是否可生图）。
func TestChatImageTriggeredByModalities(t *testing.T) {
	server, upstream, _ := newChatImageServer(t)

	status, payload := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model":      "gpt-5-5",
		"modalities": []any{"text", "image"},
		"messages":   []any{map[string]any{"role": "user", "content": "画一只猫"}},
	})
	if status == http.StatusOK {
		t.Fatalf("status = 200, want an image-chain error (body: %s)", payload)
	}
	if !strings.Contains(string(payload), "unsupported image model") {
		t.Fatalf("body = %s, want the image chain's unsupported-model error", payload)
	}
	if requests, _ := upstream.snapshot(); len(requests) != 0 {
		t.Errorf("upstream calls = %d, want 0 (model rejected before generation)", len(requests))
	}
}

// 参考图要送进上游（data URL → base64 三元组）。
func TestChatImageForwardsReferenceImages(t *testing.T) {
	server, upstream, _ := newChatImageServer(t)

	status, payload := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model": "gpt-image-2",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "把它改成蓝色"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64," + chatTestPNGBase64,
				}},
			}},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	_, images := upstream.snapshot()
	if len(images) != 1 {
		t.Fatalf("upstream images = %d, want 1", len(images))
	}
	if images[0][0] != chatTestPNGBase64 {
		t.Errorf("image payload = %q, want the data URL payload", images[0][0])
	}
	if images[0][2] != "image/png" {
		t.Errorf("image mime = %q", images[0][2])
	}
}

// 敏感词必须在取号之前拦截（不消耗账号额度），且返回 400。
func TestChatImagePromptGoesThroughContentFilter(t *testing.T) {
	server, upstream, _ := newChatImageServer(t, func(deps *api.Deps) {
		deps.ContentFilter = service.NewContentFilter(service.ContentFilterOptions{
			SensitiveWords: func() []string { return []string{"违禁内容"} },
			AIReview:       func() map[string]any { return map[string]any{"enabled": false} },
		})
	})

	status, payload := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model":    "gpt-image-2",
		"messages": []any{map[string]any{"role": "user", "content": "请画违禁内容"}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	if !strings.Contains(string(payload), "敏感词") {
		t.Errorf("body = %s, want it to mention 敏感词", payload)
	}
	requests, _ := upstream.snapshot()
	if len(requests) != 0 {
		t.Errorf("upstream calls = %d, want 0 (filtered request must not consume quota)", len(requests))
	}
}

// 上游拒图（只回文本、没有图）时，非流式把文本放进 content 而不是报 4xx。
//
// Python 的 chat_image_args 不传 message_as_error（默认 False），
// 因此 message 会以 choices[0].message.content 的形式返回。
func TestChatImageReturnsUpstreamMessageAsContent(t *testing.T) {
	server, upstream, _ := newChatImageServer(t)
	upstream.mu.Lock()
	// 没有 turn_use_case=image gen，也没有图片 id → 走「有 message 就返回 message」分支。
	upstream.sseLines = []string{`{"conversation_id":"conv-1","type":"server_ste_metadata","metadata":{}}`, `[DONE]`}
	upstream.conversation = map[string]any{}
	upstream.message = "抱歉，这个内容画不了"
	upstream.mu.Unlock()

	status, payload := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model":    "gpt-image-2",
		"messages": []any{map[string]any{"role": "user", "content": "画一只猫"}},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	choices, _ := body["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %d", len(choices))
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "抱歉，这个内容画不了" {
		t.Errorf("content = %#v, want the upstream message text", message["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %#v", choice["finish_reason"])
	}
}

// 非图片请求仍然走文本链路（不能因为接入图片形态而破坏原有行为）。
//
// 断言：没有注入 TextUpstream 时文本链路报 502（upstream factory 未配置），
// 而不是 400/501 —— 说明请求确实进了文本分支。
func TestNonImageChatStillUsesTextPath(t *testing.T) {
	deps := newTestDeps(t)
	server := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(server.Close)

	status, payload := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model":    "gpt-5-5",
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
	})
	if status == http.StatusNotImplemented {
		t.Fatalf("text chat must not be 501 anymore (body: %s)", payload)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 from the unconfigured text upstream (body: %s)", status, payload)
	}
	if !strings.Contains(string(payload), "text upstream factory is not configured") {
		t.Errorf("body = %s, want the text upstream error", payload)
	}
}

// call 日志要带上图片链路的产出 URL 与账号邮箱（日志页依赖它展示结果）。
func TestChatImageWritesCallLog(t *testing.T) {
	server, _, deps := newChatImageServer(t)

	status, payload := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model":    "gpt-image-2",
		"messages": []any{map[string]any{"role": "user", "content": "画一只猫"}},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	logs := deps.Logs.List("call", "", "", 20)
	if len(logs) == 0 {
		t.Fatal("no call log written")
	}
	latest := logs[0]
	detail, _ := latest["detail"].(map[string]any)
	if detail["endpoint"] != "/v1/chat/completions" {
		t.Errorf("endpoint = %#v", detail["endpoint"])
	}
	if detail["status"] != "success" {
		t.Errorf("status = %#v, want success", detail["status"])
	}
	if detail["account_email"] != "img@example.test" {
		t.Errorf("account_email = %#v, want img@example.test", detail["account_email"])
	}
	if urls, _ := detail["urls"].([]any); len(urls) == 0 {
		t.Errorf("urls = %#v, want at least one saved image URL", detail["urls"])
	}
}

// 流式：上游在写出任何 chunk 之前就失败 → 返回正常的 JSON 错误（不是半个 SSE）。
func TestChatImageStreamFailsBeforeFirstChunk(t *testing.T) {
	server, upstream, _ := newChatImageServer(t)
	upstream.mu.Lock()
	upstream.failStream = errors.New("upstream exploded")
	upstream.mu.Unlock()

	status, payload := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model":    "gpt-image-2",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "画一只猫"}},
	})
	if status == http.StatusOK {
		t.Fatalf("status = 200, want an error status (body: %s)", payload)
	}
	if strings.Contains(string(payload), "data: ") {
		t.Errorf("body = %s, want a JSON error rather than an SSE stream", payload)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, payload)
	}
	if _, ok := body["error"]; !ok {
		t.Errorf("body = %s, want an error envelope", payload)
	}
}

// 参考图按 Python 语义只取逆序第一条含图 user 消息的那一批（不是全部累加）。
func TestChatImageOnlyForwardsNewestImageBatch(t *testing.T) {
	server, upstream, _ := newChatImageServer(t)

	oldImage := base64.StdEncoding.EncodeToString([]byte("old-image"))
	newImage := base64.StdEncoding.EncodeToString([]byte("new-image"))
	status, payload := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model": "gpt-image-2",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "这是旧参考图"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64," + oldImage,
				}},
			}},
			map[string]any{"role": "assistant", "content": "收到"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "用这张新的"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64," + newImage,
				}},
			}},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	_, images := upstream.snapshot()
	if len(images) != 1 {
		t.Fatalf("upstream images = %d, want 1 (only the newest batch)", len(images))
	}
	if images[0][0] != newImage {
		t.Errorf("image = %q, want the newest batch (old image must be dropped)", images[0][0])
	}
}

// 多 user 消息的文本要全部拼进 prompt（用 "\n" join）。
func TestChatImagePromptJoinsAllUserMessages(t *testing.T) {
	server, upstream, _ := newChatImageServer(t)

	status, payload := postJSON(t, server, "/v1/chat/completions", map[string]any{
		"model": "gpt-image-2",
		"messages": []any{
			map[string]any{"role": "user", "content": "画一只猫"},
			map[string]any{"role": "assistant", "content": "好的"},
			map[string]any{"role": "user", "content": "背景用星空"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	requests, _ := upstream.snapshot()
	if len(requests) != 1 {
		t.Fatalf("stream requests = %d, want 1", len(requests))
	}
	if !strings.HasPrefix(requests[0].Prompt, "画一只猫\n背景用星空") {
		t.Errorf("upstream prompt = %q, want both user messages joined by newline", requests[0].Prompt)
	}
}

// parseChatSSE 抽出 data: 行（忽略 : 注释行与空行）。
func parseChatSSE(t *testing.T, payload string) []string {
	t.Helper()
	events := make([]string, 0, 8)
	scanner := bufio.NewScanner(strings.NewReader(payload))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		events = append(events, strings.TrimPrefix(line, "data: "))
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan sse: %v", err)
	}
	return events
}

// decodeChatChunks 把 data 行解成 chunk 对象。
func decodeChatChunks(t *testing.T, events []string) []map[string]any {
	t.Helper()
	chunks := make([]map[string]any, 0, len(events))
	for _, event := range events {
		if event == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(event), &chunk); err != nil {
			t.Fatalf("decode chunk: %v (%s)", err, event)
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

func chatChunkDelta(t *testing.T, chunk map[string]any) map[string]any {
	t.Helper()
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("chunk has no choices: %#v", chunk)
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		t.Fatalf("chunk has no delta: %#v", chunk)
	}
	return delta
}

func chatChunkFinishReason(t *testing.T, chunk map[string]any) any {
	t.Helper()
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("chunk has no choices: %#v", chunk)
	}
	choice, _ := choices[0].(map[string]any)
	return choice["finish_reason"]
}
