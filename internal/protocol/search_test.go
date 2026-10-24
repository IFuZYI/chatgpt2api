package protocol

import (
	"reflect"
	"testing"
)

func TestExtractSearchResultPicksLatestAssistantMessage(t *testing.T) {
	conversation := map[string]any{
		"mapping": map[string]any{
			"root": map[string]any{"message": map[string]any{
				"id":          "m-root",
				"author":      map[string]any{"role": "user"},
				"create_time": 1.0,
				"content":     map[string]any{"content_type": "text", "parts": []any{"问题"}},
			}},
			"old": map[string]any{"message": map[string]any{
				"id":          "m-old",
				"author":      map[string]any{"role": "assistant"},
				"create_time": 2.0,
				"content":     map[string]any{"content_type": "text", "parts": []any{"旧回答"}},
				"metadata": map[string]any{
					"finish_details": map[string]any{"type": "finished_successfully"},
				},
			}},
			"new": map[string]any{"message": map[string]any{
				"id":          "m-new",
				"author":      map[string]any{"role": "assistant"},
				"create_time": 3.5,
				"content":     map[string]any{"content_type": "text", "parts": []any{"新回答", "https://example.com/a."}},
				"metadata": map[string]any{
					"finish_details": map[string]any{"type": "finished_partial_completion"},
				},
			}},
		},
	}

	result := ExtractSearchResult("conv-1", conversation)
	if result.ConversationID != "conv-1" {
		t.Errorf("ConversationID = %q", result.ConversationID)
	}
	if result.AssistantMessageID != "m-new" {
		t.Errorf("AssistantMessageID = %q, want m-new", result.AssistantMessageID)
	}
	if result.CreateTime != 3.5 {
		t.Errorf("CreateTime = %v, want 3.5", result.CreateTime)
	}
	if result.Status != "finished_partial_completion" {
		t.Errorf("Status = %q", result.Status)
	}
	if result.Answer != "新回答\nhttps://example.com/a." {
		t.Errorf("Answer = %q", result.Answer)
	}
	// 正文里的 URL 追加为来源，并去掉结尾句号。
	wantSources := []SearchSource{{URL: "https://example.com/a"}}
	if !reflect.DeepEqual(result.Sources, wantSources) {
		t.Errorf("Sources = %+v, want %+v", result.Sources, wantSources)
	}
}

func TestExtractSearchResultStatusFallbacks(t *testing.T) {
	cases := []struct {
		name     string
		metadata map[string]any
		content  map[string]any
		want     string
	}{
		{
			name:     "finish_details_first",
			metadata: map[string]any{"finish_details": map[string]any{"type": "finished_successfully"}, "status": "ignored"},
			want:     "finished_successfully",
		},
		{
			name:     "metadata_status_second",
			metadata: map[string]any{"status": "in_progress"},
			want:     "in_progress",
		},
		{
			name:    "nested_status_last",
			content: map[string]any{"parts": []any{map[string]any{"status": "searching"}}},
			want:    "searching",
		},
		{
			name: "nothing",
			want: "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			message := map[string]any{
				"id":     "m1",
				"author": map[string]any{"role": "assistant"},
			}
			if testCase.metadata != nil {
				message["metadata"] = testCase.metadata
			}
			if testCase.content != nil {
				message["content"] = testCase.content
			}
			result := ExtractSearchResult("conv", map[string]any{
				"mapping": map[string]any{"node": map[string]any{"message": message}},
			})
			if result.Status != testCase.want {
				t.Errorf("Status = %q, want %q", result.Status, testCase.want)
			}
		})
	}
}

func TestExtractSearchSourcesFieldsAndDedup(t *testing.T) {
	message := map[string]any{
		"content": map[string]any{"parts": []any{
			map[string]any{"search_result_groups": []any{
				map[string]any{"entries": []any{
					map[string]any{"url": "https://a.example/1", "title": "A", "snippet": "SA", "source_type": "web"},
					map[string]any{"link": "https://b.example/2", "name": "B", "text": "SB", "type": "paper"},
					// 同一 URL 的重复项必须被丢弃（保留先出现的标题）。
					map[string]any{"source_url": "https://a.example/1", "title": "dup"},
					// metadata.url 是第四条取值路径；title 回退到 source，snippet 回退到 description。
					map[string]any{
						"metadata":    map[string]any{"url": "https://c.example/3"},
						"source":      "C",
						"description": "SC",
					},
				}},
			}},
		}},
	}

	sources := ExtractSearchSources(message)
	want := []SearchSource{
		{Title: "A", URL: "https://a.example/1", Snippet: "SA", SourceType: "web"},
		{Title: "B", URL: "https://b.example/2", Snippet: "SB", SourceType: "paper"},
		{Title: "C", URL: "https://c.example/3", Snippet: "SC"},
	}
	if !reflect.DeepEqual(sources, want) {
		t.Fatalf("sources = %+v\nwant %+v", sources, want)
	}
}

func TestExtractSearchSourcesIgnoresEmptyURLs(t *testing.T) {
	message := map[string]any{
		"content": map[string]any{"parts": []any{
			map[string]any{"title": "没有 url"},
			map[string]any{"url": "   "},
			map[string]any{"url": "https://ok.example/x"},
		}},
	}
	sources := ExtractSearchSources(message)
	if len(sources) != 1 || sources[0].URL != "https://ok.example/x" {
		t.Fatalf("sources = %+v", sources)
	}
}

func TestExtractSearchResultAppendsAnswerURLs(t *testing.T) {
	answer := "结论见 https://a.example/x. 另一处 https://b.example/y). 中文括号（https://c.example/z）"
	message := map[string]any{
		"id":      "m1",
		"author":  map[string]any{"role": "assistant"},
		"content": map[string]any{"parts": []any{answer}},
	}
	result := ExtractSearchResult("conv", map[string]any{
		"mapping": map[string]any{"node": map[string]any{"message": message}},
	})
	want := []SearchSource{
		{URL: "https://a.example/x"},
		{URL: "https://b.example/y"},
		{URL: "https://c.example/z"},
	}
	if !reflect.DeepEqual(result.Sources, want) {
		t.Fatalf("sources = %+v\nwant %+v", result.Sources, want)
	}
}

func TestExtractSearchResultDedupesAnswerURLAgainstSources(t *testing.T) {
	message := map[string]any{
		"id":     "m1",
		"author": map[string]any{"role": "assistant"},
		"metadata": map[string]any{
			"search_results": []any{
				map[string]any{"url": "https://a.example/x", "title": "A"},
			},
		},
		"content": map[string]any{"parts": []any{"引用 https://a.example/x 结束"}},
	}
	result := ExtractSearchResult("conv", map[string]any{
		"mapping": map[string]any{"node": map[string]any{"message": message}},
	})
	if len(result.Sources) != 1 || result.Sources[0].Title != "A" {
		t.Fatalf("sources = %+v", result.Sources)
	}
}

func TestSearchMessageTextJoinsParts(t *testing.T) {
	cases := []struct {
		name    string
		content any
		want    string
	}{
		{
			name: "dict_with_text_and_parts",
			content: map[string]any{
				"text": "头部",
				"parts": []any{
					"段落1",
					map[string]any{"text": "段落2"},
					map[string]any{"summary": "摘要"},
					map[string]any{"content": "正文"},
					map[string]any{"other": "忽略"},
					"   ",
				},
			},
			want: "头部\n段落1\n段落2\n摘要\n正文",
		},
		{name: "plain_string", content: "  纯文本  ", want: "纯文本"},
		{name: "missing", content: nil, want: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			message := map[string]any{"content": testCase.content}
			if testCase.content == nil {
				message = map[string]any{}
			}
			if got := SearchMessageText(message); got != testCase.want {
				t.Errorf("SearchMessageText = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestSearchMessageTextAcceptsNilMessage(t *testing.T) {
	if got := SearchMessageText(nil); got != "" {
		t.Errorf("SearchMessageText(nil) = %q", got)
	}
}

func TestFindSearchValue(t *testing.T) {
	// SSE payload 是字符串：先按正则捞，再退回 JSON 解析。
	if got := FindSearchValue(`data {"conversation_id":"cid-1"}`, "conversation_id"); got != "cid-1" {
		t.Errorf("regex path = %q", got)
	}
	// 正则只用于 conversation_id，其他 key 必须能走 JSON 递归。
	if got := FindSearchValue(`{"a":{"b":"found"}}`, "b"); got != "found" {
		t.Errorf("json path = %q", got)
	}
	if got := FindSearchValue("not-json", "b"); got != "" {
		t.Errorf("non-json = %q", got)
	}
	if got := FindSearchValue(map[string]any{"status": "", "nested": map[string]any{"status": "inner"}}, "status"); got != "inner" {
		t.Errorf("nested map = %q", got)
	}
	if got := FindSearchValue([]any{map[string]any{"x": ""}, map[string]any{"x": "second"}}, "x"); got != "second" {
		t.Errorf("list = %q", got)
	}
}

func TestCleanSearchURLTrimsTrailingPunctuation(t *testing.T) {
	cases := map[string]string{
		"https://a.example/x.": "https://a.example/x",
		// strip 在前、rstrip 在后：中间夹空格时后面的标点不会被吃掉（Python 同）。
		"  https://a.example/y。  ": "https://a.example/y",
		"https://a.example/z， 。":   "https://a.example/z， ",
		"":                         "",
	}
	for input, want := range cases {
		if got := CleanSearchURL(input); got != want {
			t.Errorf("CleanSearchURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSearchResultMapShape(t *testing.T) {
	result := SearchResult{
		ConversationID:     "cid",
		Status:             "finished_successfully",
		Answer:             "答案",
		Sources:            []SearchSource{{Title: "T", URL: "https://a.example", Snippet: "S", SourceType: "web"}},
		AssistantMessageID: "m1",
		CreateTime:         12.5,
	}
	mapped := result.Map()
	if len(mapped) != 6 {
		t.Fatalf("Map() keys = %d, want 6", len(mapped))
	}
	for _, key := range []string{"conversation_id", "status", "answer", "sources", "assistant_message_id", "create_time"} {
		if _, ok := mapped[key]; !ok {
			t.Errorf("Map() missing key %q", key)
		}
	}
	sources, ok := mapped["sources"].([]any)
	if !ok || len(sources) != 1 {
		t.Fatalf("sources = %#v", mapped["sources"])
	}
	source, ok := sources[0].(map[string]any)
	if !ok {
		t.Fatalf("source = %#v", sources[0])
	}
	if len(source) != 4 || source["url"] != "https://a.example" {
		t.Errorf("source = %#v", source)
	}
	if mapped["create_time"] != 12.5 {
		t.Errorf("create_time = %v", mapped["create_time"])
	}
}

func TestSearchResultMapEmptySourcesIsEmptySlice(t *testing.T) {
	mapped := SearchResult{}.Map()
	sources, ok := mapped["sources"].([]any)
	if !ok || len(sources) != 0 {
		t.Fatalf("sources = %#v", mapped["sources"])
	}
}
