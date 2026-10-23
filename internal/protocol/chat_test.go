package protocol

import (
	"encoding/json"
	"testing"
)

func TestNormalizeThinkingEffortForChatMatchesPython(t *testing.T) {
	cases := map[string]string{
		"":         "",
		"none":     "",
		"low":      "low",
		"MEDIUM":   "medium",
		"high":     "high",
		"xhigh":    "extended",
		"extended": "extended",
		"bogus":    "",
	}
	for input, want := range cases {
		if got := NormalizeThinkingEffortForChat(input); got != want {
			t.Errorf("NormalizeThinkingEffortForChat(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestThinkingEffortFromBodyMatchesPython(t *testing.T) {
	cases := []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"thinking_effort": "high"}, "high"},
		{map[string]any{"reasoning_effort": "xhigh"}, "extended"},
		{map[string]any{"reasoning": map[string]any{"effort": "low"}}, "low"},
		{map[string]any{}, ""},
		// thinking_effort 优先于 reasoning_effort
		{map[string]any{"thinking_effort": "low", "reasoning_effort": "high"}, "low"},
	}
	for _, tc := range cases {
		if got := ThinkingEffortFromBody(tc.body); got != tc.want {
			t.Errorf("ThinkingEffortFromBody(%v) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestChatCompletionAnnotationsMatchesPython(t *testing.T) {
	got := ChatCompletionAnnotations([]map[string]any{
		{"type": "url_citation", "start_index": 1, "end_index": 2, "url": "u", "title": "t"},
		{"type": "other"},
	})
	if len(got) != 1 {
		t.Fatalf("annotations = %v, want 1 entry", got)
	}
	entry, _ := got[0].(map[string]any)
	payload, _ := entry["url_citation"].(map[string]any)
	if payload["url"] != "u" || payload["title"] != "t" {
		t.Errorf("annotation payload wrong: %v", payload)
	}
	if payload["start_index"] != 1 || payload["end_index"] != 2 {
		t.Errorf("indices wrong: %v", payload)
	}
}

func TestChatCompletionResponseShape(t *testing.T) {
	response := ChatCompletionResponse("gpt-5", "hi", 123, 10, 5, 3, nil)

	if response["object"] != "chat.completion" {
		t.Errorf("object = %v", response["object"])
	}
	if response["created"] != int64(123) {
		t.Errorf("created = %v, want 123", response["created"])
	}
	choices, _ := response["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices len = %d", len(choices))
	}
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	message, _ := choice["message"].(map[string]any)
	if message["role"] != "assistant" || message["content"] != "hi" {
		t.Errorf("message wrong: %v", message)
	}
	// 没有 annotations 时不应出现该字段。
	if _, exists := message["annotations"]; exists {
		t.Error("annotations must be omitted when empty")
	}

	usage, _ := response["usage"].(map[string]any)
	if usage["prompt_tokens"] != 15 {
		t.Errorf("prompt_tokens = %v, want 15 (10 text + 5 image)", usage["prompt_tokens"])
	}
	if usage["completion_tokens"] != 3 {
		t.Errorf("completion_tokens = %v, want 3", usage["completion_tokens"])
	}
	if usage["total_tokens"] != 18 {
		t.Errorf("total_tokens = %v, want 18", usage["total_tokens"])
	}
}

func TestChatCompletionChunkMatchesPython(t *testing.T) {
	chunk := ChatCompletionChunk("m", map[string]any{"content": "x"}, nil, "id1", 5)
	if chunk["id"] != "id1" || chunk["object"] != "chat.completion.chunk" {
		t.Fatalf("chunk header wrong: %v", chunk)
	}
	choices, _ := chunk["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != nil {
		t.Errorf("finish_reason should be nil, got %v", choice["finish_reason"])
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta["content"] != "x" {
		t.Errorf("delta wrong: %v", delta)
	}
}

func TestChatMessagesFromBody(t *testing.T) {
	// messages 优先
	messages, err := ChatMessagesFromBody(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "a"}},
		"prompt":   "ignored",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages len = %d", len(messages))
	}
	// 回退到 prompt
	messages, err = ChatMessagesFromBody(map[string]any{"prompt": "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if messages[0]["content"] != "hello" {
		t.Errorf("prompt fallback wrong: %v", messages)
	}
	// 都没有则报错
	if _, err := ChatMessagesFromBody(map[string]any{}); err == nil {
		t.Error("expected error for empty body")
	}
}

func TestWebSearchDetection(t *testing.T) {
	if !IsWebSearchChatRequest(map[string]any{
		"tools": []any{map[string]any{"type": "web_search"}},
	}) {
		t.Error("web_search tool should trigger search path")
	}
	if !IsWebSearchChatRequest(map[string]any{"web_search_options": map[string]any{}}) {
		t.Error("web_search_options should trigger search path")
	}
	if !IsWebSearchChatRequest(map[string]any{"model": "gpt-4o-search-preview"}) {
		t.Error("search model prefix should trigger search path")
	}
	if !IsWebSearchChatRequest(map[string]any{"model": "gpt-5-search-api-2025"}) {
		t.Error("prefixed search model should trigger search path")
	}
	if IsWebSearchChatRequest(map[string]any{"model": "gpt-5"}) {
		t.Error("plain model must not trigger search path")
	}
}

func TestHasUnsupportedTools(t *testing.T) {
	allowed := WebSearchToolTypes
	if HasUnsupportedTools(map[string]any{
		"tools": []any{map[string]any{"type": "web_search"}},
	}, allowed) {
		t.Error("web_search is allowed")
	}
	if !HasUnsupportedTools(map[string]any{
		"tools": []any{map[string]any{"type": "function"}},
	}, allowed) {
		t.Error("function tool should be unsupported")
	}
	if HasUnsupportedTools(map[string]any{}, allowed) {
		t.Error("no tools means nothing unsupported")
	}
}

// 响应必须是合法 JSON（字段名是客户端契约）。
func TestChatResponseSerializes(t *testing.T) {
	response := ChatCompletionResponse("m", "x", 1, 1, 0, 1, nil)
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["object"] != "chat.completion" {
		t.Errorf("round-trip lost object field: %v", decoded)
	}
}
