package protocol

import (
	"context"
	"strings"
	"testing"
)

// tinyPNG 是 1x1 透明 PNG 的 base64（测试用的「上游返回的图片」）。
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// codexStreamFake 记录 StreamImage 的入参，并回放脚本化事件。
type codexStreamFake struct {
	fakeImageUpstream
	events   []string
	requests []ImageStreamRequest
	progress []string
}

func (f *codexStreamFake) StreamImage(ctx context.Context, request ImageStreamRequest) (<-chan string, <-chan error, error) {
	f.requests = append(f.requests, request)
	payloads := make(chan string, len(f.events))
	errs := make(chan error, 1)
	for _, event := range f.events {
		payloads <- event
	}
	close(payloads)
	close(errs)
	return payloads, errs, nil
}

// codexEventWithImage 造一条 response.output_item.done 事件（含内联 base64）。
func codexEventWithImage(b64 string) string {
	return `{"type":"response.output_item.done","item":{"type":"image_generation_call","status":"completed","result":"` + b64 + `"}}`
}

// codex 模型必须走 Responses 语义：图片内联在事件里，不轮询、不下载。
//
// 回归点：早期 codex 模型被塞进 /f/conversation，走的是 file_id + 轮询路径，
// 于是永远拿不到图（codex 响应里根本没有 file_id）。
func TestGenerateCodexImageProducesResultWithoutPolling(t *testing.T) {
	upstream := &codexStreamFake{events: []string{
		`{"type":"response.created"}`,
		codexEventWithImage(tinyPNG),
		`{"type":"response.completed","response":{"status":"completed"}}`,
	}}
	// conversations 留空：codex 路径根本不该走到轮询。
	upstream.conversations = nil

	generator := &ImageGenerator{
		Upstream: upstream,
		Config:   fastGenConfig(),
		SaveImage: func(payload []byte, baseURL string) (string, error) {
			return "http://h/images/x.png", nil
		},
	}
	outputs, err := generator.Generate(context.Background(), ImageRequest{
		Prompt: "a cat", Model: "codex-gpt-image-2", N: 1,
		ResponseFormat: "b64_json", BaseURL: "http://h",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var results []ImageOutput
	for _, output := range outputs {
		if output.Kind == ImageKindResult {
			results = append(results, output)
		}
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (outputs=%v)", len(results), outputs)
	}
	if len(results[0].Data) != 1 {
		t.Fatalf("data = %v, want 1 entry", results[0].Data)
	}
	if got := results[0].Data[0]["b64_json"]; got != tinyPNG {
		t.Errorf("b64_json = %v, want the inline result", got)
	}
	// codex 路径不产生 conversation_id，也就不该触发对话清理。
	if results[0].ConversationID != "" {
		t.Errorf("conversation id = %q, want empty for codex path", results[0].ConversationID)
	}
}

// 请求必须带 size/quality（codex 把它们放进 tools[0]，而不是提示词）。
func TestGenerateCodexImageForwardsSizeAndQuality(t *testing.T) {
	upstream := &codexStreamFake{events: []string{codexEventWithImage(tinyPNG)}}
	generator := &ImageGenerator{Upstream: upstream, Config: fastGenConfig()}

	if _, err := generator.Generate(context.Background(), ImageRequest{
		Prompt: "a cat", Model: "codex-gpt-image-2", N: 1,
		Size: "2048x2048", Quality: "high", ResponseFormat: "b64_json",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(upstream.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(upstream.requests))
	}
	request := upstream.requests[0]
	if request.Size != "2048x2048" || request.Quality != "high" {
		t.Errorf("size/quality = %q/%q, want 2048x2048/high", request.Size, request.Quality)
	}
	// codex 的 prompt 不能被塞进「输出图片尺寸为…」这类提示词（那是 web 链路的行为）。
	if strings.Contains(request.Prompt, "输出图片尺寸") {
		t.Errorf("codex prompt must not carry web-style hints, got %q", request.Prompt)
	}
	if request.Prompt != "a cat" {
		t.Errorf("prompt = %q, want unchanged %q", request.Prompt, "a cat")
	}
}

// 带套餐前缀的别名同样走 codex 语义（模型名原样传给上游用于分流）。
func TestGenerateCodexImageHandlesPrefixedModel(t *testing.T) {
	for _, model := range []string{"plus-codex-gpt-image-2", "team-codex-gpt-image-2", "pro-codex-gpt-image-2"} {
		upstream := &codexStreamFake{events: []string{codexEventWithImage(tinyPNG)}}
		generator := &ImageGenerator{Upstream: upstream, Config: fastGenConfig()}
		if _, err := generator.Generate(context.Background(), ImageRequest{
			Prompt: "x", Model: model, N: 1, ResponseFormat: "b64_json",
		}); err != nil {
			t.Errorf("%s: unexpected error: %v", model, err)
			continue
		}
		if len(upstream.requests) != 1 || upstream.requests[0].Model != model {
			t.Errorf("%s: request model = %v", model, upstream.requests)
		}
	}
}

// 响应里没有 image_generation_call → 明确报错（Python: "No image result found in response"）。
func TestGenerateCodexImageWithoutResultErrors(t *testing.T) {
	upstream := &codexStreamFake{events: []string{
		`{"type":"response.created"}`,
		`{"type":"response.completed","response":{"status":"completed","output":[]}}`,
	}}
	generator := &ImageGenerator{Upstream: upstream, Config: fastGenConfig()}

	_, err := generator.Generate(context.Background(), ImageRequest{
		Prompt: "a cat", Model: "codex-gpt-image-2", N: 1, ResponseFormat: "b64_json",
	})
	if err == nil {
		t.Fatal("expected error when no image result is present")
	}
	if !strings.Contains(err.Error(), "No image result found in response") {
		t.Errorf("error = %v, want the Python message", err)
	}
}

// data URL 前缀必须剥掉（Python: result.split(",", 1)[1]）。
func TestCodexResponseImagesStripsDataURLPrefix(t *testing.T) {
	event := map[string]any{
		"type": "response.output_item.done",
		"item": map[string]any{
			"type":   "image_generation_call",
			"result": "data:image/png;base64," + tinyPNG,
		},
	}
	images := codexResponseImages(event)
	if len(images) != 1 || images[0] != tinyPNG {
		t.Fatalf("images = %v, want stripped base64", images)
	}
}

// 同一个 item 在 done 事件与 completed.response.output 里各出现一次时只保留一份。
func TestCodexResponseImagesDeduplicates(t *testing.T) {
	payload := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"output": []any{
				map[string]any{"type": "image_generation_call", "result": tinyPNG},
			},
		},
		"item": map[string]any{"type": "image_generation_call", "result": tinyPNG},
	}
	if images := codexResponseImages(payload); len(images) != 1 {
		t.Fatalf("images = %d, want 1 after dedupe", len(images))
	}
}

// 非流式 JSON（content-type: application/json）里的结果同样要能取到。
func TestGenerateCodexImageAcceptsSingleJSONEvent(t *testing.T) {
	upstream := &codexStreamFake{events: []string{
		`{"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":"` + tinyPNG + `"}]}}`,
	}}
	generator := &ImageGenerator{Upstream: upstream, Config: fastGenConfig()}

	outputs, err := generator.Generate(context.Background(), ImageRequest{
		Prompt: "a cat", Model: "codex-gpt-image-2", N: 1, ResponseFormat: "b64_json",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, output := range outputs {
		if output.Kind == ImageKindResult {
			found = true
		}
	}
	if !found {
		t.Fatalf("no result output: %v", outputs)
	}
}

// 非 codex 模型不能被 codex 分支吞掉（web 链路行为不变）。
func TestGenerateNonCodexImageUsesWebChain(t *testing.T) {
	upstream := newImageFake()
	upstream.sseLines = []string{
		`{"conversation_id":"conv-1","type":"server_ste_metadata","metadata":{"turn_use_case":"image gen"}}`,
		`[DONE]`,
	}
	upstream.conversations = []map[string]any{conversationWithImage("img1")}

	generator := &ImageGenerator{Upstream: upstream, Config: fastGenConfig()}
	if _, err := generator.Generate(context.Background(), ImageRequest{
		Prompt: "a cat", Model: "gpt-image-2", N: 1, ResponseFormat: "b64_json",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// web 链路会把尺寸/质量拼进提示词。
	if !strings.Contains(upstream.sseLines[0], "server_ste_metadata") {
		t.Fatal("fake wiring broken")
	}
}
