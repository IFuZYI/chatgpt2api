package protocol

import (
	"regexp"
	"strings"
)

// ConversationState 是 SSE 流的状态机，字段与 Python 的 ConversationState 一致。
type ConversationState struct {
	Text           string
	RawText        string
	ConversationID string
	FileIDs        []string
	SedimentIDs    []string
	Blocked        bool
	ToolInvoked    *bool
	TurnUseCase    string
}

// 上游 SSE 里出现的标识符正则。逐字取自 conversation.py。
var (
	conversationIDRe = regexp.MustCompile(`"conversation_id"\s*:\s*"([^"]+)"`)
	fileServiceIDRe  = regexp.MustCompile(`file-service://([A-Za-z0-9_-]+)`)
	// 真正的图片文件 ID 格式：file_00000000 + 24 位十六进制（共 32 字符）。
	// 用于过滤 file_upload_business_upsell 这类非图片 ID。
	//
	// 注意：Python 的 conversation.py 里还有一个 FILE_ID_RE（带 (?!service) 负向
	// 前瞻），但它在 conversation.py 中**从未被使用**，只在 openai_backend_api.py
	// 的可编辑文件链路里用。Go 的 regexp 不支持前瞻，所以这里不移植它；
	// 可编辑文件链路在 M4 用等价的显式判断实现。
	realImageFileIDRe = regexp.MustCompile(`\bfile_00000000[a-f0-9]{24}\b`)
	sedimentIDRe      = regexp.MustCompile(`sediment://([A-Za-z0-9_-]+)`)

	// 富文本注解标记使用私用区字符 U+E200 / U+E201 / U+E202。
	// Go 的 regexp 不支持 \x{...}，直接写字面字符。
	// Go 的 regexp 不支持前瞻，所以把「后跟标点」也捕进来，在回调里再拆。
	// 捕获组：1=前导空白 2=注解内容 3=紧随的标点（可能为空）
	annotationBeforePunctRe = regexp.MustCompile("(\\s*)\uE200([^\uE201]*)\uE201([.,;:!?]?)")
	annotationRe            = regexp.MustCompile("\uE200([^\uE201]*)\uE201")
	annotationTrailingRe    = regexp.MustCompile("\uE200[^\uE201]*$")
	annotationSeparator     = "\uE202"

	turnMarkerRe  = regexp.MustCompile(`(?i)^turn\d+[a-z]*\d*$`)
	turnMarker2Re = regexp.MustCompile(`(?i)^turn\d+\w*$`)
)

// ExtractConversationIDs 从原始 payload 里抽 conversation_id / file_id / sediment_id。
func ExtractConversationIDs(payload string) (conversationID string, fileIDs []string, sedimentIDs []string) {
	if match := conversationIDRe.FindStringSubmatch(payload); len(match) > 1 {
		conversationID = match[1]
	}
	fileIDs = make([]string, 0, 2)
	addUniqueMatches(&fileIDs, fileServiceIDRe.FindAllStringSubmatch(payload, -1))
	// REAL_IMAGE_FILE_ID_RE 没有捕获组，用 FindAllString。
	for _, id := range realImageFileIDRe.FindAllString(payload, -1) {
		appendUnique(&fileIDs, id)
	}
	sedimentIDs = make([]string, 0, 2)
	addUniqueMatches(&sedimentIDs, sedimentIDRe.FindAllStringSubmatch(payload, -1))
	return conversationID, fileIDs, sedimentIDs
}

// addUniqueMatches 把正则捕获组的结果去重追加。
func addUniqueMatches(target *[]string, matches [][]string) {
	for _, match := range matches {
		if len(match) < 2 || match[1] == "" {
			continue
		}
		appendUnique(target, match[1])
	}
}

func appendUnique(target *[]string, value string) {
	if value == "" {
		return
	}
	for _, existing := range *target {
		if existing == value {
			return
		}
	}
	*target = append(*target, value)
}

// AssistantMessageText 复刻 assistant_message_text：
// 优先拼 parts 里的字符串，回退到 content.text（content_type=code 用这个）。
func AssistantMessageText(message map[string]any) string {
	content, _ := message["content"].(map[string]any)
	if content == nil {
		return ""
	}
	if parts, ok := content["parts"].([]any); ok && len(parts) > 0 {
		var builder strings.Builder
		for _, part := range parts {
			if text, ok := part.(string); ok {
				builder.WriteString(text)
			}
		}
		if builder.Len() > 0 {
			return builder.String()
		}
	}
	if text, ok := content["text"].(string); ok && text != "" {
		return text
	}
	return ""
}

// IsVisibleAssistantMessage 判断这条上游 assistant 消息是否面向用户。
//
// 过滤三类：
//   - 被标为 is_visually_hidden_from_conversation 的
//   - 发给特定 recipient 的（如 "web"，那是工具指令，不是回答）
//   - channel 不是 final 的（推理等内部通道）
func IsVisibleAssistantMessage(message map[string]any) bool {
	author, _ := message["author"].(map[string]any)
	if author == nil {
		return false
	}
	if strings.ToLower(strings.TrimSpace(stringOf(author["role"]))) != "assistant" {
		return false
	}
	if metadata, ok := message["metadata"].(map[string]any); ok {
		if hidden, ok := metadata["is_visually_hidden_from_conversation"].(bool); ok && hidden {
			return false
		}
	}
	if recipient := strings.ToLower(strings.TrimSpace(stringOf(message["recipient"]))); recipient != "" && recipient != "all" {
		return false
	}
	if channel := strings.ToLower(strings.TrimSpace(stringOf(message["channel"]))); channel != "" && channel != "final" {
		return false
	}
	return true
}

// StripHistory 去掉回放的历史前缀。
func StripHistory(text, historyText string) string {
	for historyText != "" && strings.HasPrefix(text, historyText) {
		text = text[len(historyText):]
	}
	return text
}

// SanitizeOutputText 把上游的富文本注解标记转成可读文本。
//
// 标记格式：U+E200 <kind> U+E202 <data...> U+E201
//   - kind=url  → "label (url)"
//   - kind=cite → 第一个可读部分
//
// 内部指针（turn1、source 等）直接删掉。
func SanitizeOutputText(text string) string {
	if !strings.ContainsRune(text, '\uE200') {
		return text
	}

	isInternalPart := func(part string) bool {
		value := strings.TrimSpace(part)
		if value == "" {
			return true
		}
		lower := strings.ToLower(value)
		return turnMarkerRe.MatchString(lower) ||
			turnMarker2Re.MatchString(lower) ||
			strings.HasPrefix(lower, "turn") ||
			strings.HasPrefix(lower, "source") ||
			strings.HasPrefix(lower, "sources")
	}

	readablePart := func(parts []string) string {
		for _, part := range parts {
			value := strings.TrimSpace(part)
			if value != "" && !isInternalPart(value) {
				return value
			}
		}
		return ""
	}

	annotationText := func(payload string) string {
		rawParts := strings.Split(payload, annotationSeparator)
		parts := make([]string, 0, len(rawParts))
		for _, part := range rawParts {
			parts = append(parts, strings.TrimSpace(part))
		}
		kind := ""
		if len(parts) > 0 {
			kind = strings.ToLower(parts[0])
		}
		data := parts[1:]
		if kind == "url" {
			label := ""
			url := ""
			if len(data) > 0 {
				label = data[0]
			}
			if len(data) > 1 {
				url = data[1]
			}
			if label != "" && (strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")) {
				return label + " (" + url + ")"
			}
			if label != "" {
				return label
			}
			return url
		}
		return readablePart(data)
	}

	// 先处理「注解紧邻标点」的情况：标点要保留，且注解替换后不能留空格。
	text = annotationBeforePunctRe.ReplaceAllStringFunc(text, func(match string) string {
		groups := annotationBeforePunctRe.FindStringSubmatch(match)
		if len(groups) < 4 {
			return match
		}
		leadingSpace := groups[1]
		replacement := annotationText(groups[2])
		punctuation := groups[3]
		if punctuation == "" {
			// 后面不是标点：交回给通用规则处理（返回原样，避免吞掉内容）。
			return match
		}
		if replacement == "" {
			return punctuation
		}
		return leadingSpace + replacement + punctuation
	})
	text = annotationRe.ReplaceAllStringFunc(text, func(match string) string {
		groups := annotationRe.FindStringSubmatch(match)
		if len(groups) < 2 {
			return match
		}
		return annotationText(groups[1])
	})
	text = annotationTrailingRe.ReplaceAllString(text, "")
	return text
}

// ApplyTextPatch 复刻 apply_text_patch：处理 /message/content/parts/0 的增量补丁。
func ApplyTextPatch(event map[string]any, currentText, historyText string) string {
	if stringOf(event["p"]) == "/message/content/parts/0" {
		return ApplyPatchOp(event, currentText, historyText)
	}

	operations := event["v"]
	if text, ok := operations.(string); ok && currentText != "" &&
		stringOf(event["p"]) == "" && stringOf(event["o"]) == "" {
		return currentText + text
	}

	if stringOf(event["o"]) == "patch" {
		if items, ok := operations.([]any); ok {
			text := currentText
			for _, item := range items {
				if nested, ok := item.(map[string]any); ok {
					text = ApplyTextPatch(nested, text, historyText)
				}
			}
			return text
		}
	}

	items, ok := operations.([]any)
	if !ok {
		return currentText
	}
	text := currentText
	for _, item := range items {
		if nested, ok := item.(map[string]any); ok {
			text = ApplyTextPatch(nested, text, historyText)
		}
	}
	return text
}

// ApplyPatchOp 复刻 apply_patch_op：只认 append 与 replace。
func ApplyPatchOp(operation map[string]any, currentText, historyText string) string {
	value := stringOf(operation["v"])
	switch stringOf(operation["o"]) {
	case "append":
		return currentText + value
	case "replace":
		return StripHistory(value, historyText)
	default:
		return currentText
	}
}

// AssistantRawText 复刻 assistant_raw_text：先找完整 message，再退回补丁。
func AssistantRawText(event map[string]any, currentText, historyText string) string {
	candidates := []map[string]any{event}
	if nested, ok := event["v"].(map[string]any); ok {
		candidates = append(candidates, nested)
	}
	for _, candidate := range candidates {
		message, ok := candidate["message"].(map[string]any)
		if !ok || !IsVisibleAssistantMessage(message) {
			continue
		}
		if text := AssistantMessageText(message); text != "" {
			return StripHistory(text, historyText)
		}
	}
	return ApplyTextPatch(event, currentText, historyText)
}

// AssistantText 复刻 assistant_text：raw + sanitize。
func AssistantText(event map[string]any, currentText, historyText string) string {
	return SanitizeOutputText(AssistantRawText(event, currentText, historyText))
}

// EventAssistantText 复刻 event_assistant_text：只看完整 message，不做补丁。
func EventAssistantText(event map[string]any, historyText string) string {
	candidates := []map[string]any{event}
	if nested, ok := event["v"].(map[string]any); ok {
		candidates = append(candidates, nested)
	}
	for _, candidate := range candidates {
		message, ok := candidate["message"].(map[string]any)
		if !ok || !IsVisibleAssistantMessage(message) {
			continue
		}
		return StripHistory(AssistantMessageText(message), historyText)
	}
	return ""
}

// IsImageToolEvent 判断事件是否是「图片生成工具」的完整消息。
func IsImageToolEvent(event map[string]any) bool {
	message, ok := event["message"].(map[string]any)
	if !ok {
		if nested, ok := event["v"].(map[string]any); ok {
			message, _ = nested["message"].(map[string]any)
		}
	}
	if message == nil {
		return false
	}
	metadata, _ := message["metadata"].(map[string]any)
	author, _ := message["author"].(map[string]any)
	content, _ := message["content"].(map[string]any)
	if stringOf(author["role"]) != "tool" {
		return false
	}
	if stringOf(metadata["async_task_type"]) == "image_gen" {
		return true
	}
	if stringOf(content["content_type"]) != "multimodal_text" {
		return false
	}
	parts, _ := content["parts"].([]any)
	for _, part := range parts {
		item, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if stringOf(item["content_type"]) == "image_asset_pointer" {
			return true
		}
		pointer := stringOf(item["asset_pointer"])
		if strings.HasPrefix(pointer, "file-service://") || strings.HasPrefix(pointer, "sediment://") {
			return true
		}
	}
	return false
}

// isUserMessageEvent 判断事件是否来自 user 角色（用户上传的参考图不能被当成生成结果）。
func isUserMessageEvent(event map[string]any) bool {
	message, ok := event["message"].(map[string]any)
	if !ok {
		if nested, ok := event["v"].(map[string]any); ok {
			message, _ = nested["message"].(map[string]any)
		}
	}
	if message == nil {
		return false
	}
	author, _ := message["author"].(map[string]any)
	return strings.ToLower(strings.TrimSpace(stringOf(author["role"]))) == "user"
}

// UpdateConversationState 复刻 update_conversation_state。
//
// file_id 的接受条件（三条任一）：
//  1. 事件本身是完整的 image_gen 工具消息
//  2. 之前 server_ste_metadata 已把 tool_invoked 置 true，且事件不是用户消息
//  3. patch 事件且 payload 里出现 asset_pointer / file-service://，且不是用户消息
//
// 第 2、3 条都排除用户消息：用户消息里带的是**上传的输入图**，
// 把它当生成结果会让 API 返回用户自己上传的图。
func UpdateConversationState(state *ConversationState, payload string, event map[string]any) {
	conversationID, fileIDs, sedimentIDs := ExtractConversationIDs(payload)
	if conversationID != "" && state.ConversationID == "" {
		state.ConversationID = conversationID
	}

	isPatchEvent := event != nil && stringOf(event["o"]) == "patch"
	isUserMsg := event != nil && isUserMessageEvent(event)
	imageContext := (event != nil && IsImageToolEvent(event)) ||
		(state.ToolInvoked != nil && *state.ToolInvoked && !isUserMsg) ||
		(isPatchEvent && !isUserMsg &&
			(strings.Contains(payload, "asset_pointer") || strings.Contains(payload, "file-service://")))
	if imageContext {
		for _, id := range fileIDs {
			appendUnique(&state.FileIDs, id)
		}
		for _, id := range sedimentIDs {
			appendUnique(&state.SedimentIDs, id)
		}
	}

	if event == nil {
		return
	}
	if value := stringOf(event["conversation_id"]); value != "" {
		state.ConversationID = value
	}
	if nested, ok := event["v"].(map[string]any); ok {
		if value := stringOf(nested["conversation_id"]); value != "" {
			state.ConversationID = value
		}
	}
	if stringOf(event["type"]) == "moderation" {
		if moderation, ok := event["moderation_response"].(map[string]any); ok {
			if blocked, ok := moderation["blocked"].(bool); ok && blocked {
				state.Blocked = true
			}
		}
	}
	if stringOf(event["type"]) == "server_ste_metadata" {
		if metadata, ok := event["metadata"].(map[string]any); ok {
			if invoked, ok := metadata["tool_invoked"].(bool); ok {
				value := invoked
				state.ToolInvoked = &value
			}
			if useCase := stringOf(metadata["turn_use_case"]); useCase != "" {
				state.TurnUseCase = useCase
			}
		}
	}
}

// ConversationEvent 是对外事件。Type 取值与 Python 一致：
// conversation.delta / conversation.event / conversation.raw / conversation.done。
func ConversationEvent(eventType string, state *ConversationState, extra map[string]any) map[string]any {
	toolInvoked := any(nil)
	if state.ToolInvoked != nil {
		toolInvoked = *state.ToolInvoked
	}
	event := map[string]any{
		"type":            eventType,
		"text":            state.Text,
		"conversation_id": state.ConversationID,
		"file_ids":        append([]string{}, state.FileIDs...),
		"sediment_ids":    append([]string{}, state.SedimentIDs...),
		"blocked":         state.Blocked,
		"tool_invoked":    toolInvoked,
		"turn_use_case":   state.TurnUseCase,
	}
	for key, value := range extra {
		event[key] = value
	}
	return event
}
