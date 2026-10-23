package protocol

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestStreamTextChatCompletionEmitsRoleFirst(t *testing.T) {
	chunks, err := StreamTextChatCompletion("m", "id1", 5, func(ctx context.Context, onDelta func(string)) error {
		onDelta("hello")
		onDelta(" world")
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (role + delta + stop)", len(chunks))
	}

	// 首块必须带 role
	first := chunkDelta(t, chunks[0])
	if first["role"] != "assistant" || first["content"] != "hello" {
		t.Errorf("first chunk wrong: %v", first)
	}
	// 第二块只有 content
	second := chunkDelta(t, chunks[1])
	if _, hasRole := second["role"]; hasRole {
		t.Error("second chunk must not repeat role")
	}
	if second["content"] != " world" {
		t.Errorf("second chunk content = %v", second["content"])
	}
	// 末块 finish_reason=stop 且 delta 为空
	lastChoices, _ := chunks[2]["choices"].([]any)
	last, _ := lastChoices[0].(map[string]any)
	if last["finish_reason"] != "stop" {
		t.Errorf("last finish_reason = %v", last["finish_reason"])
	}
	delta, _ := last["delta"].(map[string]any)
	if len(delta) != 0 {
		t.Errorf("last delta should be empty, got %v", delta)
	}
}

// 上游零输出时仍要发 role 空块 + stop（否则客户端一直等首块）。
func TestStreamTextChatCompletionEmitsEmptyRole(t *testing.T) {
	chunks, err := StreamTextChatCompletion("m", "id1", 5, func(ctx context.Context, onDelta func(string)) error {
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2 (empty role + stop)", len(chunks))
	}
	first := chunkDelta(t, chunks[0])
	if first["role"] != "assistant" || first["content"] != "" {
		t.Errorf("empty role chunk wrong: %v", first)
	}
}

func TestStreamTextChatCompletionPropagatesError(t *testing.T) {
	sentinel := errors.New("upstream boom")
	_, err := StreamTextChatCompletion("m", "id1", 5, func(ctx context.Context, onDelta func(string)) error {
		onDelta("partial")
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
}

func TestCollectChatContent(t *testing.T) {
	chunks, _ := StreamTextChatCompletion("m", "id1", 5, func(ctx context.Context, onDelta func(string)) error {
		onDelta("a")
		onDelta("b")
		onDelta("c")
		return nil
	})
	if got := CollectChatContent(chunks); got != "abc" {
		t.Errorf("CollectChatContent = %q, want abc", got)
	}
}

func TestTextChatPartsInjectsToolWarning(t *testing.T) {
	body := map[string]any{
		"model": "gpt-5",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"tools": []any{map[string]any{"type": "function"}},
	}
	settings := CacheSettings{NormalizeMessages: true}
	model, messages, err := TextChatParts(body, settings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "gpt-5" {
		t.Errorf("model = %q", model)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2 (injected system + user)", len(messages))
	}
	if stringOf(messages[0]["role"]) != "system" {
		t.Errorf("first message should be system, got %v", messages[0])
	}
	if stringOf(messages[0]["content"]) != ToolUnavailableSystemMessage {
		t.Errorf("system content wrong: %q", messages[0]["content"])
	}
}

// 只有受支持的工具时不应注入警告。
func TestTextChatPartsNoWarningForSearchTool(t *testing.T) {
	body := map[string]any{
		"model":    "gpt-5",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":    []any{map[string]any{"type": "web_search"}},
	}
	_, messages, err := TextChatParts(body, CacheSettings{NormalizeMessages: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1 (no injected warning)", len(messages))
	}
}

func TestMessageText(t *testing.T) {
	cases := []struct {
		input any
		want  string
	}{
		{"plain", "plain"},
		{[]any{"a", "b"}, "ab"},
		{[]any{
			map[string]any{"type": "text", "text": "x"},
			map[string]any{"type": "input_text", "text": "y"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "u"}},
		}, "xy"},
		{123, ""},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := MessageText(tc.input); got != tc.want {
			t.Errorf("MessageText(%v) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func chunkDelta(t *testing.T, chunk map[string]any) map[string]any {
	t.Helper()
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		t.Fatal("chunk has no choices")
	}
	first, _ := choices[0].(map[string]any)
	delta, _ := first["delta"].(map[string]any)
	if delta == nil {
		t.Fatal("chunk has no delta")
	}
	return delta
}

// fakeUpstream 模拟一条固定 SSE 流，用于测 StreamTextDeltas。
type fakeUpstream struct {
	payloads []string
	auth     bool
	err      error
}

func (f *fakeUpstream) ChatRequirements(ctx context.Context) (Requirements, error) {
	return Requirements{Token: "t"}, nil
}

// DoStream 按真实 SSE 格式回放：每条 payload 前面加 "data: "。
// 直接拼原始 JSON 会被 readSSE 当成非 data 行忽略 —— 这是第一次踩的坑。
func (f *fakeUpstream) DoStream(ctx context.Context, path string, payload []byte, req Requirements) (readCloser, error) {
	lines := make([]string, 0, len(f.payloads))
	for _, item := range f.payloads {
		lines = append(lines, "data: "+item)
	}
	return newStringReadCloser(strings.Join(lines, "\n") + "\n"), nil
}

func (f *fakeUpstream) Authenticated() bool { return f.auth }

func (f *fakeUpstream) UploadImage(payload []byte, filename, mime string) (map[string]any, error) {
	return nil, errors.New("not implemented")
}

func TestStreamTextDeltasAccumulatesText(t *testing.T) {
	upstream := &fakeUpstream{payloads: []string{
		`{"p":"/message/content/parts/0","o":"append","v":"Hello"}`,
		`{"p":"/message/content/parts/0","o":"append","v":" world"}`,
		`[DONE]`,
	}}
	var out strings.Builder
	err := StreamTextDeltas(context.Background(), upstream,
		ConversationTextRequest{Model: "auto", Messages: []map[string]any{{"role": "user", "content": "hi"}}},
		func(delta string) { out.WriteString(delta) })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.String() != "Hello world" {
		t.Errorf("text = %q, want 'Hello world'", out.String())
	}
}

// moderation 事件必须让整次请求失败。
func TestStreamTextDeltasDetectsBlocked(t *testing.T) {
	upstream := &fakeUpstream{payloads: []string{
		`{"type":"moderation","moderation_response":{"blocked":true}}`,
		`[DONE]`,
	}}
	err := StreamTextDeltas(context.Background(), upstream,
		ConversationTextRequest{Model: "auto", Messages: []map[string]any{{"role": "user", "content": "hi"}}},
		func(string) {})
	if err == nil {
		t.Fatal("expected error for blocked moderation")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("error should mention blocked: %v", err)
	}
}
