package protocol

import (
	"strings"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

// Token 计数用 tiktoken-go，配离线 loader 避免运行时下载 BPE 文件。
var initLoader sync.Once

// encodingForModel 复刻 encoding_for_model：
// 先按模型名找编码，失败退 o200k_base，再失败退 cl100k_base。
func encodingForModel(model string) *tiktoken.Tiktoken {
	initLoader.Do(func() {
		tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	})

	name := strings.TrimSpace(model)
	if name != "" {
		if encoding, err := tiktoken.EncodingForModel(name); err == nil && encoding != nil {
			return encoding
		}
	}
	if encoding, err := tiktoken.GetEncoding("o200k_base"); err == nil && encoding != nil {
		return encoding
	}
	encoding, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		return nil
	}
	return encoding
}

// CountTextTokens 复刻 count_text_tokens。
func CountTextTokens(text, model string) int {
	encoding := encodingForModel(model)
	if encoding == nil {
		return 0
	}
	return len(encoding.Encode(text, nil, nil))
}

// CountMessageTextTokens 复刻 count_message_text_tokens。
//
// 每条消息固定 +3，另加 name 字段的 +1，最后整体 +3。
// 这些常量是 OpenAI 的计费口径，不是估算。
func CountMessageTextTokens(messages []map[string]any, model string) int {
	encoding := encodingForModel(model)
	if encoding == nil {
		return 0
	}
	total := 0
	for _, message := range messages {
		total += 3
		for key, value := range message {
			switch typed := value.(type) {
			case []any:
				if key == "content" {
					total += len(encoding.Encode(MessageText(typed), nil, nil))
				}
			case string:
				total += len(encoding.Encode(typed, nil, nil))
			}
			if key == "name" {
				total += 1
			}
		}
	}
	return total + 3
}

// CountMessageImageTokens 复刻 count_message_image_tokens：
// 对每条消息的 content 里图片部分求 token 和。
func CountMessageImageTokens(messages []map[string]any, model string) int {
	total := 0
	for _, message := range messages {
		total += countImageContentTokens(message["content"], model)
	}
	return total
}

// countImageContentTokens 复刻 count_image_content_tokens。
func countImageContentTokens(content any, model string) int {
	parts, ok := content.([]any)
	if !ok {
		return 0
	}
	total := 0
	for _, part := range parts {
		entry, ok := part.(map[string]any)
		if !ok {
			continue
		}
		partType := strings.TrimSpace(stringOf(entry["type"]))
		if partType != "image" && partType != "image_url" && partType != "input_image" {
			if entry["source"] == nil {
				continue
			}
		}
		width, height, ok := partSize(entry)
		if !ok {
			continue
		}
		detail := strings.TrimSpace(stringOf(entry["detail"]))
		if detail == "" {
			detail = "auto"
		}
		total += CountImageInputTokens(width, height, model, detail)
	}
	return total
}

// partSize 复刻 _part_size：优先显式 width/height，其次 data 字节，最后 data URL。
func partSize(part map[string]any) (int, int, bool) {
	width := intValueOf(part["width"])
	height := intValueOf(part["height"])
	if width > 0 && height > 0 {
		return width, height, true
	}
	if data, ok := part["data"].([]byte); ok {
		return imageSizeFromBytes(data)
	}
	imageURL := part["image_url"]
	if entry, ok := imageURL.(map[string]any); ok {
		imageURL = entry["url"]
		if imageURL == nil {
			imageURL = entry["image_url"]
		}
	}
	if text, ok := imageURL.(string); ok && strings.HasPrefix(text, "data:") {
		return imageSizeFromDataURL(text)
	}
	if source, ok := part["source"].(map[string]any); ok && stringOf(source["type"]) == "base64" {
		if payload, err := decodeBase64String(stringOf(source["data"])); err == nil {
			return imageSizeFromBytes(payload)
		}
	}
	return 0, 0, false
}

// imageSizeFromDataURL 从 data URL 解出宽高。
func imageSizeFromDataURL(value string) (int, int, bool) {
	_, payload, found := strings.Cut(value, ",")
	if !found {
		return 0, 0, false
	}
	decoded, err := decodeBase64String(payload)
	if err != nil {
		return 0, 0, false
	}
	return imageSizeFromBytes(decoded)
}
