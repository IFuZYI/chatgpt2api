package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/api"
	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/IFuZYI/chatgpt2api/internal/service"
)

// newResponseImageServer 建一个注入了假图片上游的 /v1/responses 测试服务器。
//
// 复用 chat 图片链路的 chatImageUpstream 替身：两条链路走的是同一个
// ImageGenerator，上游接口契约完全一致（SSE → 对话文档 → 下载）。
func newResponseImageServer(t *testing.T, configure ...func(*api.Deps)) (*httptest.Server, *chatImageUpstream, *api.Deps) {
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

// responseImageBody 造一个 Responses 图片请求（tools 里带 image_generation）。
func responseImageBody(stream bool) map[string]any {
	return map[string]any{
		"model": "gpt-image-2",
		"input": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "画一只猫"},
			}},
		},
		"tools":  []any{map[string]any{"type": "image_generation"}},
		"stream": stream,
	}
}

// 非流式：200 + response 对象，output 是 image_generation_call（含 result）。
//
// 回归点：这条路径此前固定返回 501。
func TestResponsesImageNonStreamReturnsOutputItem(t *testing.T) {
	server, upstream, _ := newResponseImageServer(t)

	status, payload := postJSON(t, server, "/v1/responses", responseImageBody(false))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, payload)
	}
	if body["object"] != "response" {
		t.Errorf("object = %#v", body["object"])
	}
	if body["status"] != "completed" {
		t.Errorf("status = %#v", body["status"])
	}
	if id, _ := body["id"].(string); !strings.HasPrefix(id, "resp_") {
		t.Errorf("id = %#v, want resp_ prefix", body["id"])
	}
	if body["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %#v", body["parallel_tool_calls"])
	}

	output, _ := body["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %d, want 1", len(output))
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "image_generation_call" {
		t.Errorf("item type = %#v", item["type"])
	}
	if item["status"] != "completed" {
		t.Errorf("item status = %#v", item["status"])
	}
	if item["id"] != "ig_1" {
		t.Errorf("item id = %#v, want ig_1", item["id"])
	}
	if item["result"] != chatTestPNGBase64 {
		t.Errorf("item result = %#v, want the generated image base64", item["result"])
	}
	if item["revised_prompt"] != "画一只猫" {
		t.Errorf("revised_prompt = %#v, want the request prompt", item["revised_prompt"])
	}
	if _, ok := body["usage"].(map[string]any); !ok {
		t.Errorf("usage = %#v, want an object", body["usage"])
	}

	// prompt 要真的送到上游。
	requests, _ := upstream.snapshot()
	if len(requests) != 1 {
		t.Fatalf("stream requests = %d, want 1", len(requests))
	}
	if !strings.HasPrefix(requests[0].Prompt, "画一只猫") {
		t.Errorf("upstream prompt = %q", requests[0].Prompt)
	}
}

// 流式：created 先出，随后每张图一条 output_item.done，最后 completed + [DONE]。
func TestResponsesImageStreamEventOrder(t *testing.T) {
	server, _, _ := newResponseImageServer(t)

	status, raw := postJSON(t, server, "/v1/responses", responseImageBody(true))
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

	payloads := decodeChatChunks(t, events[:len(events)-1])
	types := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		types = append(types, stringValueOfTest(payload["type"]))
	}
	want := []string{"response.created", "response.output_item.done", "response.completed"}
	if len(types) != len(want) {
		t.Fatalf("event types = %v, want %v", types, want)
	}
	for index := range want {
		if types[index] != want[index] {
			t.Fatalf("event types = %v, want %v", types, want)
		}
	}

	// Python 的图片分支不产 output_item.added / partial_image。
	for _, eventType := range types {
		if strings.Contains(eventType, "partial_image") || strings.HasSuffix(eventType, "output_item.added") {
			t.Errorf("unexpected event %q: Python's image branch does not emit it", eventType)
		}
	}

	item, _ := payloads[1]["item"].(map[string]any)
	if item["type"] != "image_generation_call" || item["result"] != chatTestPNGBase64 {
		t.Errorf("item = %#v", item)
	}

	completed, _ := payloads[2]["response"].(map[string]any)
	output, _ := completed["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("completed output = %d, want 1", len(output))
	}
}

// 空 prompt 报 400（Python 的 response_events 抛 HTTPException(400)）。
func TestResponsesImageEmptyInputReturns400(t *testing.T) {
	server, upstream, _ := newResponseImageServer(t)

	status, payload := postJSON(t, server, "/v1/responses", map[string]any{
		"model":  "gpt-image-2",
		"input":  []any{map[string]any{"role": "user", "content": "   "}},
		"tools":  []any{map[string]any{"type": "image_generation"}},
		"stream": false,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	if !strings.Contains(string(payload), "input text is required") {
		t.Errorf("body = %s, want input text is required", payload)
	}
	if requests, _ := upstream.snapshot(); len(requests) != 0 {
		t.Errorf("upstream calls = %d, want 0", len(requests))
	}
}

// tool_choice 里的 image_generation 也要走图片分支（与 Python 一致）。
func TestResponsesImageBranchTriggeredByToolChoice(t *testing.T) {
	server, upstream, _ := newResponseImageServer(t)

	status, payload := postJSON(t, server, "/v1/responses", map[string]any{
		"model": "gpt-image-2",
		"input": "画一只猫",
		"tool_choice": map[string]any{
			"type": "image_generation",
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	if requests, _ := upstream.snapshot(); len(requests) != 1 {
		t.Errorf("upstream calls = %d, want 1 (tool_choice must select the image branch)", len(requests))
	}
}

// 参考图要送进上游（input_image 的 data URL → base64 三元组）。
func TestResponsesImageForwardsReferenceImage(t *testing.T) {
	server, upstream, _ := newResponseImageServer(t)

	body := responseImageBody(false)
	body["input"] = []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "把它改成蓝色"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + chatTestPNGBase64},
		}},
	}
	status, payload := postJSON(t, server, "/v1/responses", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	_, images := upstream.snapshot()
	if len(images) != 1 {
		t.Fatalf("upstream images = %d, want 1", len(images))
	}
	if images[0][0] != chatTestPNGBase64 {
		t.Errorf("image payload mismatch")
	}
	if images[0][1] != "image.png" {
		t.Errorf("filename = %q, want image.png", images[0][1])
	}
	if images[0][2] != "image/png" {
		t.Errorf("mime = %q", images[0][2])
	}
}

// 参考图嵌套在消息 content 里时同样要送上游。
func TestResponsesImageForwardsNestedReferenceImage(t *testing.T) {
	server, upstream, _ := newResponseImageServer(t)

	body := responseImageBody(false)
	body["input"] = []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "参考这张图"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + chatTestPNGBase64},
		}},
	}
	status, payload := postJSON(t, server, "/v1/responses", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	if _, images := upstream.snapshot(); len(images) != 1 {
		t.Errorf("upstream images = %d, want 1", len(images))
	}
}

// tool.size / tool.quality 要透传给生图链路（拼进上游提示词）。
func TestResponsesImageForwardsToolOptions(t *testing.T) {
	server, upstream, _ := newResponseImageServer(t)

	body := responseImageBody(false)
	body["tools"] = []any{map[string]any{
		"type":    "image_generation",
		"size":    "1024x1024",
		"quality": "high",
	}}
	status, payload := postJSON(t, server, "/v1/responses", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	requests, _ := upstream.snapshot()
	if len(requests) != 1 {
		t.Fatalf("stream requests = %d, want 1", len(requests))
	}
	// 尺寸/质量是拼进提示词的（build_image_prompt），不是独立参数。
	if !strings.Contains(requests[0].Prompt, "输出图片尺寸为 1024x1024") {
		t.Errorf("upstream prompt = %q, want the size hint", requests[0].Prompt)
	}
	if !strings.Contains(requests[0].Prompt, "输出图片质量为 high") {
		t.Errorf("upstream prompt = %q, want the quality hint", requests[0].Prompt)
	}
}

// 敏感词必须在取号之前拦截（不消耗账号额度），且返回 400。
func TestResponsesImagePromptGoesThroughContentFilter(t *testing.T) {
	server, upstream, _ := newResponseImageServer(t, func(deps *api.Deps) {
		deps.ContentFilter = service.NewContentFilter(service.ContentFilterOptions{
			SensitiveWords: func() []string { return []string{"违禁内容"} },
			AIReview:       func() map[string]any { return map[string]any{"enabled": false} },
		})
	})

	body := responseImageBody(false)
	body["input"] = "请画违禁内容"
	status, payload := postJSON(t, server, "/v1/responses", body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	if !strings.Contains(string(payload), "敏感词") {
		t.Errorf("body = %s, want it to mention 敏感词", body)
	}
	if requests, _ := upstream.snapshot(); len(requests) != 0 {
		t.Errorf("upstream calls = %d, want 0 (filtered request must not consume quota)", len(requests))
	}
}

// 上游拒图（只回文本、没有图）→ message 变成 output_text 事件，而不是 4xx。
//
// Python 的图片分支不传 message_as_error，所以拒图要以文本回复的形式出现。
func TestResponsesImageReturnsUpstreamMessageAsText(t *testing.T) {
	server, upstream, _ := newResponseImageServer(t)
	upstream.mu.Lock()
	upstream.sseLines = []string{`{"conversation_id":"conv-1","type":"server_ste_metadata","metadata":{}}`, `[DONE]`}
	upstream.conversation = map[string]any{}
	upstream.message = "抱歉，这个内容画不了"
	upstream.mu.Unlock()

	status, payload := postJSON(t, server, "/v1/responses", responseImageBody(false))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, payload)
	}
	output, _ := body["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %d, want 1", len(output))
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "message" {
		t.Fatalf("item type = %#v, want message", item["type"])
	}
	content, _ := item["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %#v", item["content"])
	}
	part, _ := content[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "抱歉，这个内容画不了" {
		t.Errorf("content part = %#v", part)
	}
}

// 流式拒图：delta → done → output_item.done → completed。
func TestResponsesImageStreamMessageEvents(t *testing.T) {
	server, upstream, _ := newResponseImageServer(t)
	upstream.mu.Lock()
	upstream.sseLines = []string{`{"conversation_id":"conv-1","type":"server_ste_metadata","metadata":{}}`, `[DONE]`}
	upstream.conversation = map[string]any{}
	upstream.message = "抱歉，这个内容画不了"
	upstream.mu.Unlock()

	status, raw := postJSON(t, server, "/v1/responses", responseImageBody(true))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}

	events := parseChatSSE(t, string(raw))
	payloads := decodeChatChunks(t, events[:len(events)-1])
	types := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		types = append(types, stringValueOfTest(payload["type"]))
	}
	want := []string{
		"response.created",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(types) != len(want) {
		t.Fatalf("event types = %v, want %v", types, want)
	}
	for index := range want {
		if types[index] != want[index] {
			t.Fatalf("event types = %v, want %v", types, want)
		}
	}
	if payloads[1]["delta"] != "抱歉，这个内容画不了" {
		t.Errorf("delta = %#v", payloads[1]["delta"])
	}
	if payloads[2]["text"] != "抱歉，这个内容画不了" {
		t.Errorf("text = %#v", payloads[2]["text"])
	}
}

// 没有注入图片上游时报 502（不是 501，也不是 200）。
func TestResponsesImageWithoutUpstreamFails(t *testing.T) {
	deps := newTestDeps(t)
	server := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(server.Close)

	status, payload := postJSON(t, server, "/v1/responses", responseImageBody(false))
	if status == http.StatusNotImplemented {
		t.Fatalf("image tool must not be 501 anymore (body: %s)", payload)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", status, payload)
	}
	if !strings.Contains(string(payload), "image upstream factory is not configured") {
		t.Errorf("body = %s, want the image upstream error", payload)
	}
}

// 不带 image_generation 的请求仍然走文本链路（不能因为接入图片形态而破坏它）。
func TestResponsesTextPathUnaffected(t *testing.T) {
	deps := newTestDeps(t)
	server := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(server.Close)

	status, payload := postJSON(t, server, "/v1/responses", map[string]any{
		"model": "auto",
		"input": "你好",
	})
	if status == http.StatusNotImplemented {
		t.Fatalf("text responses must not be 501 (body: %s)", payload)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 from the unconfigured text upstream (body: %s)", status, payload)
	}
	if !strings.Contains(string(payload), "text upstream factory is not configured") {
		t.Errorf("body = %s, want the text upstream error", payload)
	}
}

// 参考图解码失败报 400（Python 的 _decode_json_image_string 抛 HTTPException(400)）。
func TestResponsesImageRejectsBrokenReferenceImage(t *testing.T) {
	server, upstream, _ := newResponseImageServer(t)

	body := responseImageBody(false)
	body["input"] = []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "把它改成蓝色"},
			map[string]any{"type": "input_image", "b64_json": "!!!not-base64!!!", "mime": "image/png"},
		}},
	}
	status, payload := postJSON(t, server, "/v1/responses", body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, payload)
	}
	if requests, _ := upstream.snapshot(); len(requests) != 0 {
		t.Errorf("upstream calls = %d, want 0", len(requests))
	}
}

// Python 的 response_events 不读 n：这条链路永远只生成一张图。
//
// 回归点：Go 的 ImageRequest.N 零值是 0，池子会把 0 当「1 张」处理还是当
// 「0 张」处理取决于实现。body 里塞 n=3 也必须只出一张。
func TestResponsesImageIgnoresRequestN(t *testing.T) {
	server, upstream, _ := newResponseImageServer(t)

	body := responseImageBody(false)
	body["n"] = float64(3)
	status, payload := postJSON(t, server, "/v1/responses", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	if requests, _ := upstream.snapshot(); len(requests) != 1 {
		t.Errorf("upstream calls = %d, want 1 (Python defaults n to 1)", len(requests))
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode body: %v (%s)", err, payload)
	}
	output, _ := decoded["output"].([]any)
	if len(output) != 1 {
		t.Errorf("output = %d, want 1", len(output))
	}
}

// 图片响应里不能出现内部字段（_account_email 等只进 call 日志）。
func TestResponsesImageStripsInternalFields(t *testing.T) {
	server, _, _ := newResponseImageServer(t)

	status, payload := postJSON(t, server, "/v1/responses", responseImageBody(false))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	if strings.Contains(string(payload), "_account_email") {
		t.Errorf("body leaks an internal field: %s", payload)
	}
}
