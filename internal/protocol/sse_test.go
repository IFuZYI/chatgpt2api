package protocol

import "testing"

// 这些期望值全部由 Python 的 sanitize_output_text 实测产出。
func TestSanitizeOutputTextMatchesPython(t *testing.T) {
	cases := []struct{ input, want string }{
		{"hello world", "hello world"},
		{"text\uE200url\uE202label\uE202https://example.com\uE201after", "textlabel (https://example.com)after"},
		{"text\uE200url\uE202label\uE202https://example.com\uE201. next", "textlabel (https://example.com). next"},
		{"text\uE200cite\uE202turn1\uE202real content\uE201end", "textreal contentend"},
		{"text\uE200cite\uE202turn12\uE201end", "textend"},
		{"text\uE200url\uE202https://example.com\uE201", "texthttps://example.com"},
		{"abc\uE200turn1\uE201", "abc"},
		{"before\uE200url\uE202L\uE202https://x.com\uE201, after", "beforeL (https://x.com), after"},
	}
	for _, tc := range cases {
		if got := SanitizeOutputText(tc.input); got != tc.want {
			t.Errorf("SanitizeOutputText(%q)\n  got: %q\n want: %q", tc.input, got, tc.want)
		}
	}
}

func TestStripHistoryMatchesPython(t *testing.T) {
	if got := StripHistory("abcabcXYZ", "abc"); got != "XYZ" {
		t.Errorf("StripHistory = %q, want XYZ", got)
	}
	if got := StripHistory("XYZ", ""); got != "XYZ" {
		t.Errorf("empty history should not change text, got %q", got)
	}
	if got := StripHistory("XYZ", "abc"); got != "XYZ" {
		t.Errorf("non-matching history should not change text, got %q", got)
	}
}

func TestApplyPatchOpMatchesPython(t *testing.T) {
	if got := ApplyPatchOp(map[string]any{"o": "append", "v": "X"}, "AB", ""); got != "ABX" {
		t.Errorf("append = %q, want ABX", got)
	}
	if got := ApplyPatchOp(map[string]any{"o": "replace", "v": "NEW"}, "AB", ""); got != "NEW" {
		t.Errorf("replace = %q, want NEW", got)
	}
	// 未知 op 保持原文本。
	if got := ApplyPatchOp(map[string]any{"o": "delete", "v": "X"}, "AB", ""); got != "AB" {
		t.Errorf("unknown op = %q, want AB", got)
	}
}

func TestApplyTextPatchMatchesPython(t *testing.T) {
	if got := ApplyTextPatch(map[string]any{
		"p": "/message/content/parts/0", "o": "append", "v": "Z",
	}, "AB", ""); got != "ABZ" {
		t.Errorf("parts/0 append = %q, want ABZ", got)
	}
	// 无 p 无 o 的裸字符串 v 直接拼接。
	if got := ApplyTextPatch(map[string]any{"v": "MORE"}, "AB", ""); got != "ABMORE" {
		t.Errorf("plain concat = %q, want ABMORE", got)
	}
	// o=patch 时按顺序应用嵌套操作。
	got := ApplyTextPatch(map[string]any{
		"o": "patch",
		"v": []any{
			map[string]any{"p": "/message/content/parts/0", "o": "append", "v": "1"},
			map[string]any{"p": "/message/content/parts/0", "o": "append", "v": "2"},
		},
	}, "X", "")
	if got != "X12" {
		t.Errorf("nested patch = %q, want X12", got)
	}
}

func TestAssistantMessageText(t *testing.T) {
	// parts 优先
	message := map[string]any{
		"content": map[string]any{
			"parts": []any{"hello", map[string]any{"x": 1}, " world"},
			"text":  "ignored",
		},
	}
	if got := AssistantMessageText(message); got != "hello world" {
		t.Errorf("parts text = %q, want 'hello world'", got)
	}
	// content_type=code 时用 text 字段
	codeMessage := map[string]any{
		"content": map[string]any{
			"parts": []any{},
			"text":  "code body",
		},
	}
	if got := AssistantMessageText(codeMessage); got != "code body" {
		t.Errorf("code text = %q, want 'code body'", got)
	}
	if got := AssistantMessageText(map[string]any{}); got != "" {
		t.Errorf("empty message should be empty, got %q", got)
	}
}

func TestIsVisibleAssistantMessage(t *testing.T) {
	base := func(overrides map[string]any) map[string]any {
		message := map[string]any{
			"author":  map[string]any{"role": "assistant"},
			"content": map[string]any{"parts": []any{"hi"}},
		}
		for key, value := range overrides {
			message[key] = value
		}
		return message
	}

	if !IsVisibleAssistantMessage(base(nil)) {
		t.Error("plain assistant message should be visible")
	}
	if IsVisibleAssistantMessage(base(map[string]any{
		"metadata": map[string]any{"is_visually_hidden_from_conversation": true},
	})) {
		t.Error("visually hidden message must be filtered")
	}
	if IsVisibleAssistantMessage(base(map[string]any{"recipient": "web"})) {
		t.Error("tool recipient message must be filtered")
	}
	if !IsVisibleAssistantMessage(base(map[string]any{"recipient": "all"})) {
		t.Error("recipient=all should pass")
	}
	if IsVisibleAssistantMessage(base(map[string]any{"channel": "analysis"})) {
		t.Error("non-final channel must be filtered")
	}
	if !IsVisibleAssistantMessage(base(map[string]any{"channel": "final"})) {
		t.Error("channel=final should pass")
	}
	if IsVisibleAssistantMessage(map[string]any{
		"author": map[string]any{"role": "user"},
	}) {
		t.Error("user message is not an assistant message")
	}
}

func TestExtractConversationIDs(t *testing.T) {
	payload := `{"conversation_id":"conv-1","x":"file-service://abc_123",` +
		`"y":"file_00000000abcdef0123456789abcdef01","z":"sediment://sed-1"}`

	conversationID, fileIDs, sedimentIDs := ExtractConversationIDs(payload)
	if conversationID != "conv-1" {
		t.Errorf("conversation_id = %q, want conv-1", conversationID)
	}
	// file-service:// 的 id 与 file_00000000... 都应被收进来。
	if len(fileIDs) != 2 {
		t.Fatalf("file_ids = %v, want 2 entries", fileIDs)
	}
	if len(sedimentIDs) != 1 || sedimentIDs[0] != "sed-1" {
		t.Errorf("sediment_ids = %v", sedimentIDs)
	}
}

// 非图片文件 ID 必须被过滤（Python 用 REAL_IMAGE_FILE_ID_RE 卡格式）。
func TestExtractConversationIDsFiltersNonImageFileIDs(t *testing.T) {
	payload := `"file_upload_business_upsell" and "file_0000000012345678abcdef0123456789"`
	_, fileIDs, _ := ExtractConversationIDs(payload)
	if len(fileIDs) != 1 {
		t.Fatalf("file_ids = %v, want only the real image id", fileIDs)
	}
	if fileIDs[0] != "file_0000000012345678abcdef0123456789" {
		t.Errorf("wrong id kept: %v", fileIDs)
	}
}

func TestIsImageToolEvent(t *testing.T) {
	// async_task_type=image_gen 直接判定
	if !IsImageToolEvent(map[string]any{
		"message": map[string]any{
			"author":   map[string]any{"role": "tool"},
			"metadata": map[string]any{"async_task_type": "image_gen"},
		},
	}) {
		t.Error("async_task_type=image_gen should be an image tool event")
	}
	// 非 tool 角色不算
	if IsImageToolEvent(map[string]any{
		"message": map[string]any{
			"author":   map[string]any{"role": "assistant"},
			"metadata": map[string]any{"async_task_type": "image_gen"},
		},
	}) {
		t.Error("assistant message must not be an image tool event")
	}
	// multimodal_text + asset_pointer 算
	if !IsImageToolEvent(map[string]any{
		"message": map[string]any{
			"author": map[string]any{"role": "tool"},
			"content": map[string]any{
				"content_type": "multimodal_text",
				"parts": []any{
					map[string]any{"content_type": "image_asset_pointer"},
				},
			},
		},
	}) {
		t.Error("multimodal_text with image_asset_pointer should count")
	}
}

// 用户上传的参考图绝不能进 file_ids —— 否则 API 会把用户的输入图当结果返回。
func TestUpdateStateRejectsUserMessageImages(t *testing.T) {
	state := &ConversationState{}
	invoked := true
	state.ToolInvoked = &invoked

	userEvent := map[string]any{
		"message": map[string]any{
			"author": map[string]any{"role": "user"},
			"content": map[string]any{
				"parts": []any{
					map[string]any{"asset_pointer": "file-service://uploaded"},
				},
			},
		},
	}
	UpdateConversationState(state, `"asset_pointer":"file-service://uploaded"`, userEvent)
	if len(state.FileIDs) != 0 {
		t.Fatalf("user message image leaked into file_ids: %v", state.FileIDs)
	}
}

func TestUpdateStateTracksModerationAndMetadata(t *testing.T) {
	state := &ConversationState{}
	UpdateConversationState(state, `{"type":"moderation"}`, map[string]any{
		"type":                "moderation",
		"moderation_response": map[string]any{"blocked": true},
	})
	if !state.Blocked {
		t.Error("blocked should be true after moderation event")
	}

	UpdateConversationState(state, `{"type":"server_ste_metadata"}`, map[string]any{
		"type": "server_ste_metadata",
		"metadata": map[string]any{
			"tool_invoked":  true,
			"turn_use_case": "multimodal",
		},
	})
	if state.ToolInvoked == nil || !*state.ToolInvoked {
		t.Error("tool_invoked should be true")
	}
	if state.TurnUseCase != "multimodal" {
		t.Errorf("turn_use_case = %q, want multimodal", state.TurnUseCase)
	}
}
