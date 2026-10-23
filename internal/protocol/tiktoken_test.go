package protocol

import "testing"

// 期望值由 Python 的 tiktoken 实测产出。
func TestCountTextTokensMatchesPython(t *testing.T) {
	cases := []struct {
		text  string
		model string
		want  int
	}{
		{"hello world", "gpt-5", 2},
		{"hello world", "gpt-4", 2},
		{"", "gpt-5", 0},
		{"你好，世界", "gpt-5", 3},
	}
	for _, tc := range cases {
		if got := CountTextTokens(tc.text, tc.model); got != tc.want {
			t.Errorf("CountTextTokens(%q, %q) = %d, want %d", tc.text, tc.model, got, tc.want)
		}
	}
}

func TestCountMessageTextTokens(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": "hello world"},
	}
	// 1 条消息：3（基础）+ 1（role="user" 的 token 数）+ 2（"hello world"）+ 3（收尾）
	got := CountMessageTextTokens(messages, "gpt-5")
	if got <= 3 {
		t.Fatalf("CountMessageTextTokens = %d, expected > 3 (per-message overhead)", got)
	}
	// 空消息列表只算收尾的 3。
	if got := CountMessageTextTokens(nil, "gpt-5"); got != 3 {
		t.Errorf("empty messages = %d, want 3", got)
	}
}

// 没有图片的消息不应产生图片 token。
func TestCountMessageImageTokensZeroForText(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": "just text"},
	}
	if got := CountMessageImageTokens(messages, "gpt-5"); got != 0 {
		t.Errorf("image tokens = %d, want 0", got)
	}
}
