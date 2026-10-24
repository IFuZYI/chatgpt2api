package protocol

import (
	"strings"
	"testing"
)

// 引用标记必须清成可读文本，且 sources 追加成编号列表。
//
// Python: services/protocol/web_search_tool.py:88-154。
func TestCleanSearchTextReplacesURLAnnotation(t *testing.T) {
	// \ue200url\ue202OpenAI\ue202https://openai.com\ue201
	input := "答案在这里" + annotationOpen + "url" + annotationSep + "OpenAI" + annotationSep + "https://openai.com" + annotationClose + "。"

	got := CleanSearchText(input)

	if strings.Contains(got, annotationOpen) || strings.Contains(got, annotationClose) {
		t.Errorf("annotation markers not stripped: %q", got)
	}
	if !strings.Contains(got, "OpenAI (https://openai.com)") {
		t.Errorf("url annotation = %q, want 'OpenAI (https://openai.com)'", got)
	}
}

// 未闭合的尾部标记必须整段删掉（上游偶发截断）。
func TestCleanSearchTextDropsUnclosedTail(t *testing.T) {
	input := "正文" + annotationOpen + "url" + annotationSep + "半截"

	got := CleanSearchText(input)

	if strings.Contains(got, annotationOpen) {
		t.Errorf("unclosed marker not stripped: %q", got)
	}
	if !strings.HasPrefix(got, "正文") {
		t.Errorf("body lost: %q", got)
	}
}

// cite 标记取第一个有意义的片段，跳过 turn/source/数字。
func TestCleanSearchTextSkipsTurnAndDigits(t *testing.T) {
	input := "文本" + annotationOpen + "cite" + annotationSep + "turn0" + annotationSep + "42" + annotationSep + "真实内容" + annotationClose

	got := CleanSearchText(input)

	if !strings.Contains(got, "真实内容") {
		t.Errorf("cite = %q, want it to contain 真实内容", got)
	}
	if strings.Contains(got, "turn0") || strings.Contains(got, "42") {
		t.Errorf("cite kept locator noise: %q", got)
	}
}

// sources 追加成编号列表，annotation 下标必须精确覆盖 url。
func TestTextWithURLCitations(t *testing.T) {
	result := map[string]any{
		"answer": "这是答案。",
		"sources": []any{
			map[string]any{"title": "OpenAI", "url": "https://openai.com", "snippet": "s1"},
			map[string]any{"title": "", "url": "https://example.com", "snippet": "s2"},
			map[string]any{"title": "重复", "url": "https://openai.com"}, // 应被去重
		},
	}

	text, annotations := TextWithURLCitations(result)

	if !strings.Contains(text, "Sources:") {
		t.Fatalf("missing Sources section: %q", text)
	}
	if !strings.Contains(text, "1. OpenAI - https://openai.com") {
		t.Errorf("first source line wrong: %q", text)
	}
	// title 为空时只用 url，不加 " - "
	if !strings.Contains(text, "2. https://example.com") {
		t.Errorf("second source line wrong (empty title should not add ' - '): %q", text)
	}
	if len(annotations) != 2 {
		t.Fatalf("annotations = %d, want 2 (duplicate url deduped)", len(annotations))
	}
	// 下标必须精确指向 url
	for _, item := range annotations {
		start := item["start_index"].(int)
		end := item["end_index"].(int)
		url := item["url"].(string)
		if start < 0 || end > len(text) || start >= end {
			t.Errorf("bad range [%d,%d) for %q (len=%d)", start, end, url, len(text))
			continue
		}
		if text[start:end] != url {
			t.Errorf("range [%d,%d) = %q, want %q", start, end, text[start:end], url)
		}
	}
}

// 无 sources 时不加 Sources 段。
func TestTextWithURLCitationsWithoutSources(t *testing.T) {
	text, annotations := TextWithURLCitations(map[string]any{"answer": "只有答案"})

	if strings.Contains(text, "Sources:") {
		t.Errorf("unexpected Sources section: %q", text)
	}
	if len(annotations) != 0 {
		t.Errorf("annotations = %d, want 0", len(annotations))
	}
}

// 搜索词取最后一条 user 消息。
func TestSearchQueryFromMessages(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": "第一个问题"},
		{"role": "assistant", "content": "回答"},
		{"role": "user", "content": "最后的问题"},
	}

	if got := SearchQueryFromMessages(messages); got != "最后的问题" {
		t.Errorf("query = %q, want 最后的问题", got)
	}
}
