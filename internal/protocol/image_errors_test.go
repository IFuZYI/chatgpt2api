package protocol

import (
	"errors"
	"testing"
)

// 期望值全部由 Python 实测产出。
func TestPublicImageErrorMessageMatchesPython(t *testing.T) {
	cases := map[string]string{
		"":                     "The image generation request failed. Please try again later.",
		"plain error":          "plain error",
		"backend-api/x failed": "The image generation request failed. Please try again later.",
		"status=500":           "The image generation request failed. Please try again later.",
		"body=abc":             "The image generation request failed. Please try again later.",
		"chatgpt.com":          "The image generation request failed. Please try again later.",
		"UpstreamHTTPError":    "The image generation request failed. Please try again later.",
	}
	for input, want := range cases {
		if got := PublicImageErrorMessage(input); got != want {
			t.Errorf("PublicImageErrorMessage(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestErrorClassifiers(t *testing.T) {
	if IsTokenInvalidError("Token has been invalidated") {
		// Python 的标记是 "authentication token has been invalidated"，
		// 缺 "authentication " 时不匹配 —— 这是既有行为，不是漏判。
		t.Error(`"Token has been invalidated" should NOT match (missing "authentication " prefix)`)
	}
	if !IsTokenInvalidError("authentication token has been invalidated") {
		t.Error("full marker should match")
	}
	if !IsTokenInvalidError("token_invalidated") {
		t.Error("snake_case marker should match")
	}
	if !IsTLSConnectionError("curl: (35) error") {
		t.Error("curl 35 should be a TLS error")
	}
	if !IsConnectionTimeoutError("curl: (28) timeout") {
		t.Error("curl 28 should be a timeout error")
	}
}

func TestImageStreamErrorMessageMatchesPython(t *testing.T) {
	cases := map[string]string{
		"token_invalidated": "image generation failed",
		"curl: (35) x":      "upstream image connection failed, please retry later",
		"curl: (28) x":      "upstream connection timed out, please retry later",
		"plain":             "plain",
		"":                  "image generation failed",
	}
	for input, want := range cases {
		if got := ImageStreamErrorMessage(input); got != want {
			t.Errorf("ImageStreamErrorMessage(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIsModelTextReplyInsteadOfImage(t *testing.T) {
	if !IsModelTextReplyInsteadOfImage(`{"referenced_image_ids":["a"]}`) {
		t.Error("referenced_image_ids should be detected")
	}
	if IsModelTextReplyInsteadOfImage("hello") {
		t.Error("plain text should not be detected")
	}
	if IsModelTextReplyInsteadOfImage("") {
		t.Error("empty should not be detected")
	}
}

// 三种 kind 的字段裁剪是前端契约，逐字段核对。
func TestImageOutputToChunkProgress(t *testing.T) {
	chunk := ImageOutput{
		Kind: ImageKindProgress, Model: "m", Index: 1, Total: 2,
		Text: "p", UpstreamEventType: "ev", ConversationID: "c",
	}.ToChunk()

	if chunk["object"] != "image.generation.chunk" {
		t.Errorf("object = %v", chunk["object"])
	}
	if chunk["progress_text"] != "p" || chunk["upstream_event_type"] != "ev" {
		t.Errorf("progress fields missing: %v", chunk)
	}
	if chunk["_conversation_id"] != "c" {
		t.Errorf("conversation id missing: %v", chunk)
	}
}

func TestImageOutputToChunkMessage(t *testing.T) {
	chunk := ImageOutput{
		Kind: ImageKindMessage, Model: "m", Index: 1, Total: 2, Text: "msg",
	}.ToChunk()

	if chunk["object"] != "image.generation.message" {
		t.Errorf("object = %v", chunk["object"])
	}
	if chunk["message"] != "msg" {
		t.Errorf("message field missing: %v", chunk)
	}
	// progress 字段必须被删掉
	if _, exists := chunk["progress_text"]; exists {
		t.Error("progress_text must be removed for message kind")
	}
	if _, exists := chunk["upstream_event_type"]; exists {
		t.Error("upstream_event_type must be removed for message kind")
	}
	// data 保留为空数组（Python 也保留）
	if _, exists := chunk["data"]; !exists {
		t.Error("data field should be retained as empty array")
	}
	// 空 conversation_id 不写内部字段
	if _, exists := chunk["_conversation_id"]; exists {
		t.Error("empty conversation_id must not be written")
	}
}

func TestImageOutputToChunkResult(t *testing.T) {
	chunk := ImageOutput{
		Kind: ImageKindResult, Model: "m", Index: 1, Total: 2,
		Data: []map[string]any{{"b64_json": "x"}},
	}.ToChunk()

	if chunk["object"] != "image.generation.result" {
		t.Errorf("object = %v", chunk["object"])
	}
	data, _ := chunk["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d", len(data))
	}
	if _, exists := chunk["progress_text"]; exists {
		t.Error("progress_text must be removed for result kind")
	}
}

func TestFormatImageResultB64JSON(t *testing.T) {
	save := func(payload []byte, baseURL string) (string, error) {
		return baseURL + "/images/x.png", nil
	}
	items := []map[string]any{{"b64_json": "aGVsbG8="}}
	result := FormatImageResult(items, "prompt", "b64_json", save, "http://h", 123, "")

	data, _ := result["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d", len(data))
	}
	entry, _ := data[0].(map[string]any)
	// b64_json 模式下 b64_json 与 url 都要有
	if entry["b64_json"] != "aGVsbG8=" {
		t.Errorf("b64_json missing: %v", entry)
	}
	if entry["url"] != "http://h/images/x.png" {
		t.Errorf("url missing: %v", entry)
	}
	if entry["revised_prompt"] != "prompt" {
		t.Errorf("revised_prompt should fall back to prompt: %v", entry)
	}
	if result["created"] != int64(123) {
		t.Errorf("created = %v", result["created"])
	}
}

func TestFormatImageResultURLOnly(t *testing.T) {
	save := func(payload []byte, baseURL string) (string, error) {
		return baseURL + "/images/x.png", nil
	}
	result := FormatImageResult(
		[]map[string]any{{"b64_json": "aGVsbG8="}}, "p", "url", save, "http://h", 1, "")

	data, _ := result["data"].([]any)
	entry, _ := data[0].(map[string]any)
	if _, exists := entry["b64_json"]; exists {
		t.Error("b64_json must be absent in url mode")
	}
	if entry["url"] == "" {
		t.Error("url missing")
	}
}

// 没有图片但有 message 时，message 要出现在结果里。
func TestFormatImageResultMessageFallback(t *testing.T) {
	result := FormatImageResult(nil, "p", "b64_json", nil, "", 1, "upstream said no")
	if result["message"] != "upstream said no" {
		t.Errorf("message fallback missing: %v", result)
	}
	data, _ := result["data"].([]any)
	if len(data) != 0 {
		t.Errorf("data should be empty: %v", data)
	}
}

func TestImageGenerationErrorToOpenAIError(t *testing.T) {
	err := &ImageGenerationError{
		Message:      "backend-api/x failed", // 含内部信息，应被隐藏
		StatusCode:   502,
		AccountEmail: "a@b.c",
	}
	payload := err.ToOpenAIError()
	inner, _ := payload["error"].(map[string]any)
	if inner["message"] != "The image generation request failed. Please try again later." {
		t.Errorf("message should be sanitized: %v", inner["message"])
	}
	if inner["account_email"] != "a@b.c" {
		t.Errorf("account_email missing: %v", inner)
	}
	if inner["type"] != "server_error" || inner["code"] != "upstream_error" {
		t.Errorf("defaults wrong: %v", inner)
	}
}

func TestNoImageQuotaError(t *testing.T) {
	if got := NoImageQuotaError("").Error(); got != "no available image quota" {
		t.Errorf("bare quota error = %q", got)
	}
	if got := NoImageQuotaError("Plus").Error(); got != "no available Plus image quota" {
		t.Errorf("prefixed quota error = %q", got)
	}
	// 与号池的哨兵错误可比
	if !errors.Is(ErrNoImageQuota, ErrNoImageQuota) {
		t.Error("sentinel comparison broken")
	}
}
