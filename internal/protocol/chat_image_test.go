package protocol

import (
	"encoding/base64"
	"strings"
	"testing"
)

// pngBase64 是一张 1x1 PNG 的 base64（合法图片字节，够 imageSizeFromBytes 用）。
const chatTestPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// 直接 prompt 优先：body.prompt 非空时完全不看 messages。
func TestExtractChatPromptPrefersDirectPrompt(t *testing.T) {
	body := map[string]any{
		"prompt": "  直接提示词  ",
		"messages": []any{
			map[string]any{"role": "user", "content": "来自消息的提示词"},
		},
	}
	if got := ExtractChatPrompt(body); got != "直接提示词" {
		t.Errorf("ExtractChatPrompt = %q, want 直接提示词", got)
	}
}

// 遍历**全部** user 消息并用 "\n" 拼接（不是只取最后一条）。
//
// 回归点：只看最后一条会让多轮「先描述风格、再给具体内容」的请求丢掉前半段，
// 出图与用户预期不符。Python utils/helper.py:431-447 是累加所有 user 消息。
func TestExtractChatPromptJoinsAllUserMessages(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "你是画师"},
			map[string]any{"role": "user", "content": "画一只猫"},
			map[string]any{"role": "assistant", "content": "好的"},
			map[string]any{"role": "user", "content": "背景用星空"},
		},
	}
	want := "画一只猫\n背景用星空"
	if got := ExtractChatPrompt(body); got != want {
		t.Errorf("ExtractChatPrompt = %q, want %q", got, want)
	}
}

// text 与 input_text 两种 part 都要收；input_text 允许落在 input_text 字段。
func TestExtractChatPromptHandlesTextPartTypes(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "  第一段  "},
				map[string]any{"type": "input_text", "text": "第二段"},
				map[string]any{"type": "input_text", "input_text": "第三段"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAA"}},
				"不是对象，应被忽略",
			}},
		},
	}
	want := "第一段\n第二段\n第三段"
	if got := ExtractChatPrompt(body); got != want {
		t.Errorf("ExtractChatPrompt = %q, want %q", got, want)
	}
}

// 没有可用 prompt 时返回空串（调用方据此报 400）。
func TestExtractChatPromptEmptyWhenNoUserText(t *testing.T) {
	cases := []map[string]any{
		{},
		{"messages": "not a list"},
		{"messages": []any{map[string]any{"role": "assistant", "content": "只有助手"}}},
		{"messages": []any{map[string]any{"role": "user", "content": "   "}}},
		{"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAA"}},
		}}}},
	}
	for index, body := range cases {
		if got := ExtractChatPrompt(body); got != "" {
			t.Errorf("case %d: ExtractChatPrompt = %q, want empty", index, got)
		}
	}
}

// 逆序遍历，取**第一条**含图 user 消息的那一批；更早消息里的图必须被丢弃。
//
// 回归点：把所有消息里的图都收进来会让「先传参考图、再传新参考图」的请求
// 带上一堆历史图，上游按多图编辑处理，结果完全跑偏。
func TestExtractChatImageTakesFirstBatchFromReverse(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("old-1")),
				}},
			}},
			map[string]any{"role": "assistant", "content": "看到了"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("new-1")),
				}},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/webp;base64," + base64.StdEncoding.EncodeToString([]byte("new-2")),
				}},
			}},
		},
	}
	images, err := ExtractChatImage(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("got %d images, want 2 (only the last user message's batch)", len(images))
	}
	if images[0][0] != base64.StdEncoding.EncodeToString([]byte("new-1")) {
		t.Errorf("images[0] = %q, want new-1 (reverse order must pick the newest batch)", images[0][0])
	}
	if images[1][0] != base64.StdEncoding.EncodeToString([]byte("new-2")) {
		t.Errorf("images[1] = %q, want new-2", images[1][0])
	}
	// Python chat_image_args 的 filename 恒为 image_{idx}.png。
	if images[0][1] != "image_1.png" || images[1][1] != "image_2.png" {
		t.Errorf("filenames = %q / %q, want image_1.png / image_2.png", images[0][1], images[1][1])
	}
	if images[0][2] != "image/jpeg" || images[1][2] != "image/webp" {
		t.Errorf("mimes = %q / %q", images[0][2], images[1][2])
	}
}

// 逆序扫描时，最后一条 user 消息没有图就继续往前找（不是直接放弃）。
func TestExtractChatImageSkipsMessagesWithoutImages(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image_url", "image_url": "data:image/png;base64," + chatTestPNG},
			}},
			map[string]any{"role": "user", "content": "只改一下颜色"},
		},
	}
	images, err := ExtractChatImage(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("got %d images, want 1", len(images))
	}
	if images[0][2] != "image/png" {
		t.Errorf("mime = %q, want image/png", images[0][2])
	}
}

// input_image / image 两种 part 走 _decode_message_image_object：
// b64_json、source{type=base64}、image_url 三条路径都要通。
func TestExtractChatImageDecodesObjectForms(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("payload"))
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_image", "b64_json": encoded},
				map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "data": encoded, "media_type": "image/webp",
				}},
				map[string]any{"type": "image", "image_url": "data:image/gif;base64," + encoded},
			}},
		},
	}
	images, err := ExtractChatImage(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 3 {
		t.Fatalf("got %d images, want 3", len(images))
	}
	wantMIMEs := []string{"image/png", "image/webp", "image/gif"}
	for index, want := range wantMIMEs {
		if images[index][2] != want {
			t.Errorf("images[%d] mime = %q, want %q", index, images[index][2], want)
		}
	}
}

// 非法 base64 要报 400 语义的错误，而不是静默丢图。
func TestExtractChatImageRejectsInvalidBase64(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image", "b64_json": "!!!not-base64!!!"},
			}},
		},
	}
	if _, err := ExtractChatImage(body); err == nil {
		t.Fatal("want error for invalid base64")
	} else if err.Error() != "invalid base64 image data" {
		t.Errorf("error = %q, want invalid base64 image data", err.Error())
	}
}

// 不支持的 mime 要报错（Python 的 SUPPORTED_JSON_IMAGE_MIME_TYPES 白名单）。
func TestExtractChatImageRejectsUnsupportedMIME(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image", "b64_json": "AAAA", "mime_type": "application/pdf"},
			}},
		},
	}
	if _, err := ExtractChatImage(body); err == nil || err.Error() != "unsupported image mime type" {
		t.Fatalf("error = %v, want unsupported image mime type", err)
	}
}

// 多图 markdown 用 "\n\n" 拼接，序号从 1 开始。
func TestBuildChatImageMarkdownContentJoinsWithBlankLine(t *testing.T) {
	data := []any{
		map[string]any{"b64_json": "AAA"},
		map[string]any{"b64_json": "BBB"},
	}
	want := "![image_1](data:image/png;base64,AAA)\n\n![image_2](data:image/png;base64,BBB)"
	if got := BuildChatImageMarkdownContent(data); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// 序号按 data 下标（含被跳过的条目），无图时返回固定兜底文案。
func TestBuildChatImageMarkdownContentFallbacks(t *testing.T) {
	if got := BuildChatImageMarkdownContent(nil); got != "Image generation completed." {
		t.Errorf("empty data = %q", got)
	}
	data := []any{
		map[string]any{"url": "https://example.test/a.png"},
		"不是对象",
		map[string]any{"b64_json": "CCC"},
	}
	want := "![image_3](data:image/png;base64,CCC)"
	if got := BuildChatImageMarkdownContent(data); got != want {
		t.Errorf("got %q, want %q (index must follow the data array)", got, want)
	}
}

// image_result_content：有 data 用 markdown，否则回退 message，再否则固定文案。
func TestImageResultContent(t *testing.T) {
	withData := map[string]any{"data": []any{map[string]any{"b64_json": "AAA"}}}
	if got := ImageResultContent(withData); got != "![image_1](data:image/png;base64,AAA)" {
		t.Errorf("with data = %q", got)
	}
	withMessage := map[string]any{"data": []any{}, "message": "上游拒绝了"}
	if got := ImageResultContent(withMessage); got != "上游拒绝了" {
		t.Errorf("with message = %q", got)
	}
	if got := ImageResultContent(map[string]any{}); got != "Image generation completed." {
		t.Errorf("empty = %q", got)
	}
}

// chat_image_args：model 缺省 gpt-image-2，prompt 缺失报错，n 走 parse_image_count。
func TestChatImageArgs(t *testing.T) {
	model, prompt, n, images, err := ChatImageArgs(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "画一只猫"}},
		"n":        float64(2),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "gpt-image-2" {
		t.Errorf("model = %q, want gpt-image-2", model)
	}
	if prompt != "画一只猫" {
		t.Errorf("prompt = %q", prompt)
	}
	if n != 2 {
		t.Errorf("n = %d, want 2", n)
	}
	if len(images) != 0 {
		t.Errorf("images = %v, want none", images)
	}

	if _, _, _, _, err := ChatImageArgs(map[string]any{"model": "gpt-image-2"}); err == nil {
		t.Fatal("want error for missing prompt")
	} else if err.Error() != "prompt is required" {
		t.Errorf("error = %q, want prompt is required", err.Error())
	}
}

// parse_image_count 是 `int(raw or 1)` + 1..4 校验：与 /v1/images/generations 的
// pydantic 严格校验**不同** —— 假值（0 / "" / null / false）会被当成 1，
// 但字符串 "0" 是真值，走 int("0") = 0 后报越界（Python 同）。
func TestParseImageCount(t *testing.T) {
	ok := []struct {
		raw  any
		want int
	}{
		{"1", 1},
		{"4", 4},
		{float64(0), 1}, // 0 or 1 → 1
		{"", 1},
		{nil, 1},
		{false, 1},
		{2.9, 2},
		{" 3 ", 3},
		{true, 1},
	}
	for _, tc := range ok {
		got, err := ParseImageCount(tc.raw)
		if err != nil {
			t.Errorf("ParseImageCount(%#v) error = %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseImageCount(%#v) = %d, want %d", tc.raw, got, tc.want)
		}
	}

	bad := []any{"0", "5", "-1", "abc", float64(5), float64(-1)}
	for _, value := range bad {
		if _, err := ParseImageCount(value); err == nil {
			t.Errorf("ParseImageCount(%#v) want error", value)
		}
	}
}

// 流式状态机：首块带 role，后续不带；结尾一块 finish_reason=stop。
func TestChatImageChunkStreamRoleOnlyOnFirstChunk(t *testing.T) {
	stream := NewChatImageChunkStream("gpt-image-2")

	first, ok := stream.Chunk(ImageOutput{Kind: ImageKindProgress, Text: "正在准备"})
	if !ok {
		t.Fatal("progress chunk dropped")
	}
	if delta := firstDelta(t, first); delta["role"] != "assistant" {
		t.Errorf("first delta role = %#v, want assistant", delta["role"])
	}

	second, ok := stream.Chunk(ImageOutput{Kind: ImageKindProgress, Text: "正在绘制"})
	if !ok {
		t.Fatal("second progress chunk dropped")
	}
	if _, hasRole := firstDelta(t, second)["role"]; hasRole {
		t.Error("second chunk must not carry role")
	}

	finish := stream.Finish()
	if len(finish) != 1 {
		t.Fatalf("finish chunks = %d, want 1 (role already sent)", len(finish))
	}
	if reason := firstFinishReason(t, finish[0]); reason != "stop" {
		t.Errorf("finish_reason = %#v, want stop", reason)
	}
}

// 一条内容都没有时，Finish 要补一个空 role 块 + stop 块（Python 同）。
func TestChatImageChunkStreamEmptyFinishSendsRoleChunk(t *testing.T) {
	stream := NewChatImageChunkStream("gpt-image-2")
	finish := stream.Finish()
	if len(finish) != 2 {
		t.Fatalf("finish chunks = %d, want 2", len(finish))
	}
	if delta := firstDelta(t, finish[0]); delta["role"] != "assistant" || delta["content"] != "" {
		t.Errorf("first finish delta = %#v", delta)
	}
	if reason := firstFinishReason(t, finish[1]); reason != "stop" {
		t.Errorf("finish_reason = %#v, want stop", reason)
	}
}

// message 分支的切片语义：以已发进度文本开头就只发增量，否则整段发。
func TestChatImageChunkStreamMessageSlicesSentText(t *testing.T) {
	stream := NewChatImageChunkStream("gpt-image-2")
	if _, ok := stream.Chunk(ImageOutput{Kind: ImageKindProgress, Text: "前缀"}); !ok {
		t.Fatal("progress dropped")
	}

	chunk, ok := stream.Chunk(ImageOutput{Kind: ImageKindMessage, Text: "前缀+后续"})
	if !ok {
		t.Fatal("message dropped")
	}
	if content := firstDelta(t, chunk)["content"]; content != "+后续" {
		t.Errorf("content = %#v, want +后续 (sent prefix must be sliced off)", content)
	}

	// 不以 sentText 开头 → 整段发（Python 的三元表达式另一分支）。
	other := NewChatImageChunkStream("gpt-image-2")
	if _, ok := other.Chunk(ImageOutput{Kind: ImageKindProgress, Text: "前缀"}); !ok {
		t.Fatal("progress dropped")
	}
	chunk, ok = other.Chunk(ImageOutput{Kind: ImageKindMessage, Text: "完全不同的文本"})
	if !ok {
		t.Fatal("message dropped")
	}
	if content := firstDelta(t, chunk)["content"]; content != "完全不同的文本" {
		t.Errorf("content = %#v, want full text", content)
	}
}

// 与 sentText 完全相同的 message 不产生内容（Python 的 `if not content: continue`）。
func TestChatImageChunkStreamDropsEmptyMessage(t *testing.T) {
	stream := NewChatImageChunkStream("gpt-image-2")
	if _, ok := stream.Chunk(ImageOutput{Kind: ImageKindProgress, Text: "全部内容"}); !ok {
		t.Fatal("progress dropped")
	}
	if _, ok := stream.Chunk(ImageOutput{Kind: ImageKindMessage, Text: "全部内容"}); ok {
		t.Error("message identical to sent text must not emit a chunk")
	}
}

// result 分支直接产出 markdown。
func TestChatImageChunkStreamResultBecomesMarkdown(t *testing.T) {
	stream := NewChatImageChunkStream("gpt-image-2")
	chunk, ok := stream.Chunk(ImageOutput{
		Kind: ImageKindResult,
		Data: []map[string]any{{"b64_json": "AAA"}},
	})
	if !ok {
		t.Fatal("result dropped")
	}
	want := "![image_1](data:image/png;base64,AAA)"
	if content := firstDelta(t, chunk)["content"]; content != want {
		t.Errorf("content = %#v, want %q", content, want)
	}
}

// 非流式响应：choices[0].message.content 是 markdown，usage 走图片口径。
func TestImageChatCompletionShape(t *testing.T) {
	images := [][3]string{{chatTestPNG, "image_1.png", "image/png"}}
	result := map[string]any{
		"created": int64(1700000000),
		"data":    []any{map[string]any{"b64_json": chatTestPNG}},
	}
	response := ImageChatCompletion("gpt-image-2", result, "画一只猫", images)

	if response["object"] != "chat.completion" {
		t.Errorf("object = %#v", response["object"])
	}
	if response["created"] != int64(1700000000) {
		t.Errorf("created = %#v, want 1700000000 (from result)", response["created"])
	}
	if id, _ := response["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
		t.Errorf("id = %q", id)
	}

	choices, _ := response["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %d", len(choices))
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	want := "![image_1](data:image/png;base64," + chatTestPNG + ")"
	if message["content"] != want {
		t.Errorf("content = %#v, want markdown", message["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %#v", choice["finish_reason"])
	}

	usage, _ := response["usage"].(map[string]any)
	if usage["completion_tokens"] == 0 {
		t.Error("completion_tokens must count output image tokens")
	}
	if details, _ := usage["completion_tokens_details"].(map[string]any); intValueOf(details["image_tokens"]) == 0 {
		t.Error("completion_tokens_details.image_tokens must be non-zero")
	}
	if details, _ := usage["prompt_tokens_details"].(map[string]any); intValueOf(details["text_tokens"]) == 0 {
		t.Error("prompt_tokens_details.text_tokens must count the prompt")
	}
}

// 没有图时 content 用 message（上游拒图的场景），usage 的 completion_tokens 为 0。
func TestImageChatCompletionUsesMessageWhenNoImage(t *testing.T) {
	response := ImageChatCompletion("gpt-image-2",
		map[string]any{"created": int64(1), "data": []any{}, "message": "被上游拒绝"},
		"画一只猫", nil)

	choices, _ := response["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "被上游拒绝" {
		t.Errorf("content = %#v", message["content"])
	}
	usage, _ := response["usage"].(map[string]any)
	if usage["completion_tokens"] != 0 {
		t.Errorf("completion_tokens = %#v, want 0", usage["completion_tokens"])
	}
}

// firstDelta 取 chunk 里的 delta。
func firstDelta(t *testing.T, chunk map[string]any) map[string]any {
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

// firstFinishReason 取 chunk 里的 finish_reason。
func firstFinishReason(t *testing.T, chunk map[string]any) any {
	t.Helper()
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("chunk has no choices: %#v", chunk)
	}
	choice, _ := choices[0].(map[string]any)
	return choice["finish_reason"]
}
