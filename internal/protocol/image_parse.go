package protocol

import (
	"sort"
	"strings"
	"time"
)

// ContentPolicyKeywords 是判定「上游按内容政策拒绝」的关键词。
// 逐字取自 openai_backend_api.py 的 _CONTENT_POLICY_KEYWORDS。
var ContentPolicyKeywords = []string{
	// 明确的内容政策违规
	"内容政策", "防护限制", "违反", "moderation", "policy", "blocked",
	// 拒绝生成类
	"不能生成", "无法生成", "不能帮助", "无法帮助",
	// 敏感内容类
	"裸体", "裸露", "色情", "性内容", "未成年",
	// 通用拒绝
	"抱歉，我不能",
}

// IsContentPolicyError 复刻 _is_content_policy_error。
func IsContentPolicyError(message string) bool {
	if message == "" {
		return false
	}
	lower := strings.ToLower(message)
	for _, keyword := range ContentPolicyKeywords {
		if strings.Contains(lower, strings.ToLower(keyword)) {
			return true
		}
	}
	return false
}

// ImageToolRecord 是一条图片工具输出记录。
type ImageToolRecord struct {
	MessageID   string
	CreateTime  float64
	FileIDs     []string
	SedimentIDs []string
}

// ExtractImageReferenceIDs 复刻 _extract_image_reference_ids：
// 递归遍历任意结构，收集 file-service:// / file_00000000… / sediment://。
func ExtractImageReferenceIDs(payload any) (fileIDs []string, sedimentIDs []string) {
	fileIDs = make([]string, 0, 2)
	sedimentIDs = make([]string, 0, 2)

	var walk func(value any)
	walk = func(value any) {
		switch typed := value.(type) {
		case string:
			for _, match := range fileServiceIDRe.FindAllStringSubmatch(typed, -1) {
				if len(match) > 1 {
					appendUnique(&fileIDs, match[1])
				}
			}
			for _, id := range realImageFileIDRe.FindAllString(typed, -1) {
				appendUnique(&fileIDs, id)
			}
			for _, match := range sedimentIDRe.FindAllStringSubmatch(typed, -1) {
				if len(match) > 1 {
					appendUnique(&sedimentIDs, match[1])
				}
			}
		case map[string]any:
			for _, item := range typed {
				walk(item)
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(payload)
	return fileIDs, sedimentIDs
}

// HasImageAssetPointer 复刻 _has_image_asset_pointer。
func HasImageAssetPointer(payload any) bool {
	switch typed := payload.(type) {
	case map[string]any:
		if stringOf(typed["content_type"]) == "image_asset_pointer" {
			return true
		}
		pointer := stringOf(typed["asset_pointer"])
		if strings.HasPrefix(pointer, "file-service://") || strings.HasPrefix(pointer, "sediment://") {
			return true
		}
		for _, item := range typed {
			if HasImageAssetPointer(item) {
				return true
			}
		}
		return false
	case []any:
		for _, item := range typed {
			if HasImageAssetPointer(item) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// ExtractImageToolRecords 复刻 _extract_image_tool_records：
// 遍历 conversation.mapping，收集 tool/assistant 角色里的图片记录，按 create_time 排序。
//
// 过滤逻辑：
//   - role 必须是 tool 或 assistant
//   - assistant 消息只有在 async_task_type=image_gen 或带 asset_pointer 时才算
//     （否则普通回答文本会被误当图片记录）
func ExtractImageToolRecords(conversation map[string]any) []ImageToolRecord {
	mapping, _ := conversation["mapping"].(map[string]any)
	if mapping == nil {
		return nil
	}

	records := make([]ImageToolRecord, 0, 4)
	for messageID, rawNode := range mapping {
		node, _ := rawNode.(map[string]any)
		if node == nil {
			continue
		}
		message, _ := node["message"].(map[string]any)
		if message == nil {
			continue
		}
		author, _ := message["author"].(map[string]any)
		metadata, _ := message["metadata"].(map[string]any)
		content, _ := message["content"].(map[string]any)

		role := strings.ToLower(strings.TrimSpace(stringOf(author["role"])))
		if role != "tool" && role != "assistant" {
			continue
		}

		isImageGen := stringOf(metadata["async_task_type"]) == "image_gen"
		hasAssetPointer := HasImageAssetPointer(content) || HasImageAssetPointer(metadata)
		if role == "assistant" && !isImageGen && !hasAssetPointer {
			continue
		}

		fileIDs, sedimentIDs := ExtractImageReferenceIDs(map[string]any{
			"content":  content,
			"metadata": metadata,
		})
		if !isImageGen && !hasAssetPointer && len(fileIDs) == 0 && len(sedimentIDs) == 0 {
			continue
		}

		createTime := floatValueOf(message["create_time"])
		records = append(records, ImageToolRecord{
			MessageID:   messageID,
			CreateTime:  createTime,
			FileIDs:     fileIDs,
			SedimentIDs: sedimentIDs,
		})
	}

	sort.SliceStable(records, func(i, j int) bool {
		return records[i].CreateTime < records[j].CreateTime
	})
	return records
}

// FindContentPolicyError 复刻 _find_content_policy_error_in_conversation：
// 在 assistant/tool 消息文本里找内容政策关键词，命中则返回前 500 字符。
func FindContentPolicyError(conversation map[string]any) string {
	mapping, _ := conversation["mapping"].(map[string]any)
	if mapping == nil {
		return ""
	}
	for _, rawNode := range mapping {
		node, _ := rawNode.(map[string]any)
		if node == nil {
			continue
		}
		message, _ := node["message"].(map[string]any)
		if message == nil {
			continue
		}
		author, _ := message["author"].(map[string]any)
		role := strings.ToLower(strings.TrimSpace(stringOf(author["role"])))
		if role != "assistant" && role != "tool" {
			continue
		}

		parts := make([]string, 0, 2)
		switch content := message["content"].(type) {
		case map[string]any:
			if items, ok := content["parts"].([]any); ok {
				for _, part := range items {
					if text, ok := part.(string); ok && strings.TrimSpace(text) != "" {
						parts = append(parts, strings.TrimSpace(text))
					}
				}
			}
			if text := strings.TrimSpace(stringOf(content["text"])); text != "" {
				parts = append(parts, text)
			}
		case string:
			if strings.TrimSpace(content) != "" {
				parts = append(parts, strings.TrimSpace(content))
			}
		}

		messageText := strings.Join(parts, "\n")
		if messageText != "" && IsContentPolicyError(messageText) {
			if len(messageText) > 500 {
				return messageText[:500]
			}
			return messageText
		}
	}
	return ""
}

// TaskErrorMessage 复刻 check_task_error 里「取错误文本」的部分。
//
// 判定依据是结构化字段，不是文本匹配：
//   - metadata.is_error == true
//   - content.content_type == "text"（不是 multimodal_text）
//
// 只有三者同时满足才认为是真错误；否则上游把工具参数 JSON 标记成 is_error
// 的情况会被误判（而图片其实正在生成）。
func TaskErrorMessage(task map[string]any) string {
	imageMessage, _ := task["image_gen_message"].(map[string]any)
	if imageMessage == nil {
		return ""
	}
	metadata, _ := imageMessage["metadata"].(map[string]any)
	content, _ := imageMessage["content"].(map[string]any)
	author, _ := imageMessage["author"].(map[string]any)

	isError, _ := metadata["is_error"].(bool)
	isTextOnly := stringOf(content["content_type"]) == "text"
	isAssistantRole := stringOf(author["role"]) == "assistant"
	if !isError || !isTextOnly || !isAssistantRole {
		return ""
	}

	parts, _ := content["parts"].([]any)
	var builder strings.Builder
	for _, part := range parts {
		if text, ok := part.(string); ok {
			builder.WriteString(text)
		}
	}
	return builder.String()
}

// floatValueOf 容错取数字。
func floatValueOf(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	default:
		return 0
	}
}

// formatFloatValue 是 formatFloat 的薄别名（image_poll.go 用）。
func formatFloatValue(value float64) string { return formatFloat(value) }

// httpStatusError 是上游返回非 2xx 时的错误，携带状态码与 Retry-After。
type httpStatusError struct {
	Status     int
	RetryAfter *int
	Message    string
}

func (e *httpStatusError) Error() string { return e.Message }

// NewHTTPStatusError 构造带状态码的上游错误。
func NewHTTPStatusError(status int, retryAfter *int, message string) error {
	return &httpStatusError{Status: status, RetryAfter: retryAfter, Message: message}
}

// httpStatusOf 取出错误里的 HTTP 状态码（没有则 0）。
func httpStatusOf(err error) int {
	var statusErr *httpStatusError
	if asError(err, &statusErr) {
		return statusErr.Status
	}
	return 0
}

// retryAfterOf 取出 Retry-After（秒）。
func retryAfterOf(err error) *int {
	var statusErr *httpStatusError
	if asError(err, &statusErr) {
		return statusErr.RetryAfter
	}
	return nil
}

// isNetworkError 判断是否是可重试的网络错误。
func isNetworkError(err error) bool {
	message := err.Error()
	return IsTLSConnectionError(message) || IsConnectionTimeoutError(message) ||
		strings.Contains(strings.ToLower(message), "connection") ||
		strings.Contains(strings.ToLower(message), "timeout")
}

// NowFunc 便于测试替换时间源。
var NowFunc = time.Now

// StatusCodeOf 导出 httpStatusOf，供其他包判断上游错误类型。
func StatusCodeOf(err error) int { return httpStatusOf(err) }
