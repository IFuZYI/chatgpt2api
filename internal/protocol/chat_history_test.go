package protocol

import (
	"strings"
	"testing"
)

// HistoryTexts 必须只取 assistant 消息的文本。
func TestHistoryTexts(t *testing.T) {
	messages := []map[string]any{
		{"role": "system", "content": "be nice"},
		{"role": "user", "content": "first question"},
		{"role": "assistant", "content": "previous answer"},
		{"role": "user", "content": "second question"},
	}
	got := HistoryTexts(messages)
	if len(got) != 1 {
		t.Fatalf("history = %v, want 1 entry", got)
	}
	if got[0] != "previous answer" {
		t.Errorf("history[0] = %q", got[0])
	}
}

// 块数组形态的 assistant 内容也要能提取。
func TestHistoryTextsFromParts(t *testing.T) {
	messages := []map[string]any{
		{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "part one "},
			map[string]any{"type": "text", "text": "part two"},
		}},
	}
	got := HistoryTexts(messages)
	if len(got) != 1 || got[0] != "part one part two" {
		t.Errorf("history = %v", got)
	}
}

func TestJoinHistoryText(t *testing.T) {
	if got := joinHistoryText(nil); got != "" {
		t.Errorf("nil history = %q", got)
	}
	// Python 是 `"".join(...)`（conversation.py:210），不带分隔符。
	if got := joinHistoryText([]string{"a", "", "b"}); got != "ab" {
		t.Errorf("join = %q, want %q", got, "ab")
	}
}

// 上游重放的历史片段必须被跳过，本轮输出不能以历史开头。
//
// 回归点：不跳过时，历史 "previous answer" + 新答 "NEW" 会输出
// "previous answerNEW"（Python 只输出 "NEW"）。
func TestHistoryIsSkippedNotReplayed(t *testing.T) {
	history := []string{"previous answer"}

	// 模拟上游：先重放历史 assistant 消息，再发新回复。
	events := []map[string]any{
		{"v": map[string]any{"message": map[string]any{
			"author":   map[string]any{"role": "assistant"},
			"content":  map[string]any{"content_type": "text", "parts": []any{"previous answer"}},
			"status":   "finished_successfully",
			"metadata": map[string]any{},
		}}},
		{"v": map[string]any{"message": map[string]any{
			"author":   map[string]any{"role": "assistant"},
			"content":  map[string]any{"content_type": "text", "parts": []any{"NEW"}},
			"status":   "finished_successfully",
			"metadata": map[string]any{},
		}}},
	}

	state := &ConversationState{}
	historyText := joinHistoryText(history)
	historyIndex := 0
	var out strings.Builder

	for _, event := range events {
		UpdateConversationState(state, "", event)
		nextRaw := AssistantRawText(event, state.RawText, historyText)
		if historyIndex < len(history) && AssistantEventText(event, historyText) == history[historyIndex] {
			historyIndex++
			state.RawText = ""
			state.Text = ""
			continue
		}
		nextText := SanitizeOutputText(nextRaw)
		if nextText != state.Text {
			out.WriteString(nextText[len(state.Text):])
			state.Text = nextText
			state.RawText = nextRaw
		}
	}

	if got := out.String(); got != "NEW" {
		t.Errorf("output = %q, want %q (history must be skipped, not replayed)", got, "NEW")
	}
}
