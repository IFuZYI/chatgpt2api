package protocol

import "testing"

func TestIsContentPolicyError(t *testing.T) {
	positives := []string{
		"内容政策违规", "防护限制", "违反使用条款", "moderation blocked",
		"policy violation", "不能生成该图片", "抱歉，我不能",
	}
	for _, message := range positives {
		if !IsContentPolicyError(message) {
			t.Errorf("%q should be a content policy error", message)
		}
	}
	negatives := []string{"", "图片生成成功", "normal text"}
	for _, message := range negatives {
		if IsContentPolicyError(message) {
			t.Errorf("%q should not be a content policy error", message)
		}
	}
}

func TestExtractImageReferenceIDs(t *testing.T) {
	payload := map[string]any{
		"a": "file-service://abc_1",
		"b": []any{"file_0000000012345678abcdef0123456789", "sediment://sed_1"},
		"c": map[string]any{"d": "file-service://abc_2"},
	}
	fileIDs, sedimentIDs := ExtractImageReferenceIDs(payload)
	if len(fileIDs) != 3 {
		t.Fatalf("file_ids = %v, want 3", fileIDs)
	}
	if len(sedimentIDs) != 1 || sedimentIDs[0] != "sed_1" {
		t.Errorf("sediment_ids = %v", sedimentIDs)
	}
	// 去重
	fileIDs, _ = ExtractImageReferenceIDs("file-service://x file-service://x")
	if len(fileIDs) != 1 {
		t.Errorf("duplicates not removed: %v", fileIDs)
	}
}

func TestHasImageAssetPointer(t *testing.T) {
	if !HasImageAssetPointer(map[string]any{"content_type": "image_asset_pointer"}) {
		t.Error("content_type should match")
	}
	if !HasImageAssetPointer(map[string]any{"asset_pointer": "file-service://x"}) {
		t.Error("file-service pointer should match")
	}
	if !HasImageAssetPointer(map[string]any{"nested": map[string]any{"asset_pointer": "sediment://y"}}) {
		t.Error("nested pointer should match")
	}
	if HasImageAssetPointer(map[string]any{"text": "hello"}) {
		t.Error("plain text should not match")
	}
}

// assistant 的普通文本回答不能被当成图片记录。
func TestExtractImageToolRecordsFiltersPlainAssistant(t *testing.T) {
	conversation := map[string]any{
		"mapping": map[string]any{
			"n1": map[string]any{
				"message": map[string]any{
					"author":   map[string]any{"role": "assistant"},
					"content":  map[string]any{"parts": []any{"just an answer"}},
					"metadata": map[string]any{},
				},
			},
		},
	}
	if records := ExtractImageToolRecords(conversation); len(records) != 0 {
		t.Fatalf("plain assistant message should not be a record: %v", records)
	}
}

// tool 角色的图片记录应被收集，并按 create_time 排序。
func TestExtractImageToolRecordsCollectsAndSorts(t *testing.T) {
	conversation := map[string]any{
		"mapping": map[string]any{
			"late": map[string]any{
				"message": map[string]any{
					"author":      map[string]any{"role": "tool"},
					"create_time": float64(200),
					"content": map[string]any{
						"parts": []any{
							map[string]any{"asset_pointer": "file-service://second"},
						},
					},
					"metadata": map[string]any{"async_task_type": "image_gen"},
				},
			},
			"early": map[string]any{
				"message": map[string]any{
					"author":      map[string]any{"role": "tool"},
					"create_time": float64(100),
					"content": map[string]any{
						"parts": []any{
							map[string]any{"asset_pointer": "file-service://first"},
						},
					},
					"metadata": map[string]any{"async_task_type": "image_gen"},
				},
			},
		},
	}
	records := ExtractImageToolRecords(conversation)
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if records[0].FileIDs[0] != "first" {
		t.Errorf("records not sorted by create_time: %v", records)
	}
}

func TestFindContentPolicyError(t *testing.T) {
	conversation := map[string]any{
		"mapping": map[string]any{
			"n1": map[string]any{
				"message": map[string]any{
					"author":  map[string]any{"role": "assistant"},
					"content": map[string]any{"parts": []any{"内容政策违规，不能生成"}},
				},
			},
		},
	}
	if got := FindContentPolicyError(conversation); got == "" {
		t.Fatal("should detect content policy error")
	}
	// 用户消息不参与判定
	userConversation := map[string]any{
		"mapping": map[string]any{
			"n1": map[string]any{
				"message": map[string]any{
					"author":  map[string]any{"role": "user"},
					"content": map[string]any{"parts": []any{"内容政策违规"}},
				},
			},
		},
	}
	if got := FindContentPolicyError(userConversation); got != "" {
		t.Errorf("user message should not be scanned: %q", got)
	}
}

// 只有三个结构化条件同时满足才算真错误。
func TestTaskErrorMessage(t *testing.T) {
	valid := map[string]any{
		"image_gen_message": map[string]any{
			"metadata": map[string]any{"is_error": true},
			"content":  map[string]any{"content_type": "text", "parts": []any{"boom"}},
			"author":   map[string]any{"role": "assistant"},
		},
	}
	if got := TaskErrorMessage(valid); got != "boom" {
		t.Errorf("TaskErrorMessage = %q, want boom", got)
	}

	// content_type 是 multimodal_text 时不算错误（图片正在生成）
	multimodal := map[string]any{
		"image_gen_message": map[string]any{
			"metadata": map[string]any{"is_error": true},
			"content":  map[string]any{"content_type": "multimodal_text"},
			"author":   map[string]any{"role": "assistant"},
		},
	}
	if got := TaskErrorMessage(multimodal); got != "" {
		t.Errorf("multimodal should not be an error, got %q", got)
	}

	// 没有 image_gen_message
	if got := TaskErrorMessage(map[string]any{}); got != "" {
		t.Errorf("empty task should be empty, got %q", got)
	}
}

func TestHTTPStatusError(t *testing.T) {
	retryAfter := 5
	err := NewHTTPStatusError(429, &retryAfter, "rate limited")
	if httpStatusOf(err) != 429 {
		t.Errorf("status = %d", httpStatusOf(err))
	}
	if got := retryAfterOf(err); got == nil || *got != 5 {
		t.Errorf("retry-after = %v", got)
	}
	if httpStatusOf(NewHTTPStatusError(500, nil, "x")) != 500 {
		t.Error("status extraction broken")
	}
}
