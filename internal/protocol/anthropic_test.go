package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// 期望值全部由 Python 的 anthropic_v1_messages 实测产出。
func TestStripToolMarkupMatchesPython(t *testing.T) {
	input := "hello <tool_calls><tool_call><tool_name>f</tool_name><parameters><a>1</a></parameters></tool_call></tool_calls> tail"
	if got := StripToolMarkup(input); got != "hello  tail" {
		t.Errorf("StripToolMarkup = %q, want 'hello  tail'", got)
	}
}

func TestStreamableTextMatchesPython(t *testing.T) {
	input := "hello <tool_calls><tool_call><tool_name>f</tool_name></tool_call></tool_calls> tail"
	if got := StreamableText(input); got != "hello" {
		t.Errorf("StreamableText = %q, want 'hello'", got)
	}
	// 没有工具标记时原样返回
	if got := StreamableText("plain text"); got != "plain text" {
		t.Errorf("StreamableText(plain) = %q", got)
	}
}

func TestXMLValueMatchesPython(t *testing.T) {
	if got := XMLValue("<a><![CDATA[<x>]]></a>", "a"); got != "<x>" {
		t.Errorf("CDATA value = %q, want '<x>'", got)
	}
	if got := XMLValue("<a>hello</a>", "a"); got != "hello" {
		t.Errorf("plain value = %q", got)
	}
	if got := XMLValue("<a>hi</a>", "b"); got != "" {
		t.Errorf("missing tag should be empty, got %q", got)
	}
	// HTML 实体要解
	if got := XMLValue("<a>&lt;x&gt;</a>", "a"); got != "<x>" {
		t.Errorf("entity value = %q, want '<x>'", got)
	}
}

func TestParseToolParamsMatchesPython(t *testing.T) {
	// JSON 形式
	got := ParseToolParams(`{"k": 1}`)
	if got["k"] != float64(1) {
		t.Errorf("json params = %v", got)
	}
	// 标签形式
	got = ParseToolParams("<k>1</k><s>hi</s>")
	if got["k"] != float64(1) || got["s"] != "hi" {
		t.Errorf("tag params = %v", got)
	}
}

func TestParseToolCallsMatchesPython(t *testing.T) {
	input := "hello <tool_calls><tool_call><tool_name>f</tool_name><parameters><a>1</a></parameters></tool_call></tool_calls> tail"
	calls := ParseToolCalls(input)
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Name != "f" {
		t.Errorf("name = %q", calls[0].Name)
	}
	if calls[0].Input["a"] != float64(1) {
		t.Errorf("input = %v", calls[0].Input)
	}
}

func TestContentBlocksMatchesPython(t *testing.T) {
	input := "hello <tool_calls><tool_call><tool_name>f</tool_name><parameters><a>1</a></parameters></tool_call></tool_calls> tail"
	blocks, stopReason := ContentBlocks(input, []any{map[string]any{"name": "f"}})
	if stopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", stopReason)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (text + tool_use)", len(blocks))
	}
	if stringOf(blocks[0]["type"]) != "text" || stringOf(blocks[0]["text"]) != "hello  tail" {
		t.Errorf("text block wrong: %v", blocks[0])
	}
	if stringOf(blocks[1]["type"]) != "tool_use" || stringOf(blocks[1]["name"]) != "f" {
		t.Errorf("tool block wrong: %v", blocks[1])
	}
	if !strings.HasPrefix(stringOf(blocks[1]["id"]), "toolu_") {
		t.Errorf("tool id prefix wrong: %v", blocks[1]["id"])
	}

	// 无工具时只有文本块
	blocks, stopReason = ContentBlocks("plain", nil)
	if stopReason != "end_turn" || len(blocks) != 1 {
		t.Fatalf("plain: blocks=%d stop=%q", len(blocks), stopReason)
	}
	if stringOf(blocks[0]["text"]) != "plain" {
		t.Errorf("plain text = %v", blocks[0]["text"])
	}
}

// 这段提示词的格式有两处「看起来像 bug」但必须原样保留（见 BuildToolPrompt 注释）。
func TestBuildToolPromptExactFormat(t *testing.T) {
	got := BuildToolPrompt([]any{
		map[string]any{"name": "f", "description": "d", "input_schema": map[string]any{"type": "object"}},
	})
	want := `Available tools:
Tool: f
Description: d
Parameters: {"type":"object"}Tool use rules:
- If the user asks to list/read/search files, inspect project state, run a command, or answer from local code, you MUST call a suitable tool first. Do not say you cannot access files.
- To call tools, output ONLY XML and no prose/markdown:
<tool_calls><tool_call><tool_name>TOOL_NAME</tool_name><parameters><PARAM><![CDATA[value]]></PARAM></parameters></tool_call></tool_calls>
- Put parameters under <parameters> using the exact schema names.`

	if got != want {
		t.Errorf("tool prompt mismatch\n got: %q\nwant: %q", got, want)
	}
	// 明确断言两个反直觉点，避免以后被「顺手修好」
	if strings.Contains(got, `"object"}`+"\n"+"Tool use rules") {
		t.Error("Tool use rules must NOT be preceded by a newline (Python behaviour)")
	}
	if BuildToolPrompt(nil) != "" {
		t.Error("no tools should produce empty prompt")
	}
}

func TestMergeSystem(t *testing.T) {
	if got := MergeSystem("sys", "EXTRA"); got != "sys\n\nEXTRA" {
		t.Errorf("merge string = %v", got)
	}
	// Claude Code 的 system 会被替换成 XML_TOOL_RULE
	got := MergeSystem("You are Claude Code", "EXTRA")
	text, _ := got.(string)
	if !strings.HasPrefix(text, "You are Claude Code") || !strings.Contains(text, "Tool output adapter") {
		t.Errorf("claude code merge wrong: %q", text)
	}
	if strings.Contains(text, "EXTRA") {
		t.Error("extra should be replaced by XML_TOOL_RULE for Claude Code systems")
	}
	// 空 extra 时原样返回
	if got := MergeSystem("sys", ""); got != "sys" {
		t.Errorf("empty extra should return system unchanged, got %v", got)
	}
}

func TestHasClaudeCodeSystem(t *testing.T) {
	if !HasClaudeCodeSystem("You are Claude Code x") {
		t.Error("string system should match")
	}
	if !HasClaudeCodeSystem([]any{map[string]any{"text": "You are Claude Code"}}) {
		t.Error("list system should match")
	}
	if HasClaudeCodeSystem("other") {
		t.Error("non-matching system should not match")
	}
}

func TestMessageResponseShape(t *testing.T) {
	response := MessageResponse("m", "hi", 1, 2, nil)
	if response["type"] != "message" || response["role"] != "assistant" {
		t.Errorf("response header wrong: %v", response)
	}
	if response["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", response["stop_reason"])
	}
	usage, _ := response["usage"].(map[string]any)
	if usage["input_tokens"] != 1 || usage["output_tokens"] != 2 {
		t.Errorf("usage wrong: %v", usage)
	}
	if !strings.HasPrefix(stringOf(response["id"]), "msg_") {
		t.Errorf("id prefix wrong: %v", response["id"])
	}
}

func TestPreprocessBlockConvertsToolUse(t *testing.T) {
	block := PreprocessBlock(map[string]any{
		"type": "tool_use", "name": "f", "input": map[string]any{"a": 1},
	})
	entry, _ := block.(map[string]any)
	if stringOf(entry["type"]) != "text" {
		t.Fatalf("tool_use should become text, got %v", entry)
	}
	if !strings.Contains(stringOf(entry["text"]), "<tool_name>f</tool_name>") {
		t.Errorf("converted text wrong: %q", entry["text"])
	}

	result := PreprocessBlock(map[string]any{
		"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok",
	})
	resultEntry, _ := result.(map[string]any)
	if stringOf(resultEntry["text"]) != "Tool result toolu_1: ok" {
		t.Errorf("tool_result conversion wrong: %q", resultEntry["text"])
	}
}

func TestStreamAnthropicEventsShape(t *testing.T) {
	chunks := []map[string]any{
		ChatCompletionChunk("m", map[string]any{"role": "assistant", "content": "hi"}, nil, "id", 1),
		ChatCompletionChunk("m", map[string]any{}, "stop", "id", 1),
	}
	events := StreamAnthropicEvents(chunks, "m", 5, func(text string) int { return len(text) }, nil)

	if len(events) == 0 {
		t.Fatal("no events produced")
	}
	if events[0]["type"] != "message_start" {
		t.Errorf("first event should be message_start, got %v", events[0]["type"])
	}
	if events[len(events)-1]["type"] != "message_stop" {
		t.Errorf("last event should be message_stop, got %v", events[len(events)-1]["type"])
	}

	// 必须有 text_delta
	var sawDelta bool
	for _, event := range events {
		if event["type"] == "content_block_delta" {
			delta, _ := event["delta"].(map[string]any)
			if delta["type"] == "text_delta" && delta["text"] == "hi" {
				sawDelta = true
			}
		}
	}
	if !sawDelta {
		t.Error("missing text_delta event")
	}
}

// 事件必须能序列化成合法 JSON（Anthropic SDK 按此解析）。
func TestAnthropicEventsSerialize(t *testing.T) {
	chunks := []map[string]any{
		ChatCompletionChunk("m", map[string]any{"content": "x"}, "stop", "id", 1),
	}
	events := StreamAnthropicEvents(chunks, "m", 1, func(string) int { return 1 }, nil)
	for _, event := range events {
		if _, err := json.Marshal(event); err != nil {
			t.Errorf("event not serializable: %v (%v)", err, event)
		}
	}
}
