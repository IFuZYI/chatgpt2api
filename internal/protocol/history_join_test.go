package protocol

import (
	"strings"
	"testing"
)

// 历史前缀必须用空字符串拼接（Python `"".join`），不是 "\n\n"。
//
// 回归点：assistant 历史用 "\n\n" 拼接后，StripHistory 拿这个前缀去
// 真实输出里找 —— 真实输出里两段回答之间没有 "\n\n"，于是找不到、
// 不剥离，客户端看到 "上一轮回答" + "本轮回答" 粘在一起。
func TestJoinHistoryTextUsesEmptySeparator(t *testing.T) {
	history := []string{"first answer", "second answer"}

	got := joinHistoryText(history)
	want := "first answersecond answer"

	if got != want {
		t.Errorf("joinHistoryText = %q, want %q (Python uses \"\".join)", got, want)
	}
}

// 单条历史不受分隔符影响，但要保证不被 trim。
func TestJoinHistoryTextKeepsSingleEntryVerbatim(t *testing.T) {
	history := []string{"  spaced answer  "}

	got := joinHistoryText(history)

	if got != "  spaced answer  " {
		t.Errorf("joinHistoryText = %q, want verbatim %q (Python does not trim)",
			got, "  spaced answer  ")
	}
}

// BuildToolPrompt 的多工具之间必须有换行（Python `"\n".join(blocks)`）。
func TestBuildToolPromptSeparatesToolsWithNewline(t *testing.T) {
	tools := []any{
		map[string]any{"name": "read_file", "description": "Read a file", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"name": "list_dir", "description": "List a directory", "input_schema": map[string]any{"type": "object"}},
	}

	prompt := BuildToolPrompt(tools)

	if !strings.Contains(prompt, "read_file") || !strings.Contains(prompt, "list_dir") {
		t.Fatalf("prompt missing tool names: %q", prompt)
	}
	// 两个工具块之间必须是换行分隔，不能直接粘在一起。
	if strings.Contains(prompt, "}}}Tool: list_dir") || strings.Contains(prompt, "}}Tool: list_dir") {
		t.Errorf("tools are concatenated without a newline separator: %q", prompt)
	}
	// 找第二个工具块的起始位置（"Tool: list_dir"），它前面必须是换行。
	marker := "Tool: list_dir"
	idx := strings.Index(prompt, marker)
	if idx < 0 {
		t.Fatalf("second tool block not found in %q", prompt)
	}
	if idx == 0 || prompt[idx-1] != '\n' {
		before := prompt[max(0, idx-5):idx]
		t.Errorf("no newline before second tool block; preceding chars = %q", before)
	}
}
