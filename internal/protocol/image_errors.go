package protocol

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// 图片链路的错误类型。三者对应不同的对外状态码与文案，不能合并：
//   - ImageGenerationError → 502/429，带 account_email 供排查
//   - ImagePollTimeoutError → 超时，前端会显示「继续等待」按钮
//   - ImageContentPolicyError → 上游拒绝，不重试
type ImageGenerationError struct {
	Message        string
	StatusCode     int
	ErrorType      string
	Code           string
	Param          string
	AccountEmail   string
	ConversationID string
}

func (e *ImageGenerationError) Error() string { return e.Message }

// ToOpenAIError 复刻 ImageGenerationError.to_openai_error。
func (e *ImageGenerationError) ToOpenAIError() map[string]any {
	payload := map[string]any{
		"message": PublicImageErrorMessage(e.Message),
		"type":    orDefaultString(e.ErrorType, "server_error"),
		"param":   nilIfEmptyString(e.Param),
		"code":    orDefaultString(e.Code, "upstream_error"),
	}
	if e.AccountEmail != "" {
		payload["account_email"] = e.AccountEmail
	}
	return map[string]any{"error": payload}
}

// ImagePollTimeoutError 表示轮询预算耗尽。
type ImagePollTimeoutError struct {
	Message        string
	ConversationID string
	TaskError      string
}

func (e *ImagePollTimeoutError) Error() string { return e.Message }

// ImageContentPolicyError 表示上游按内容政策拒绝。
type ImageContentPolicyError struct {
	Message        string
	ConversationID string
}

func (e *ImageContentPolicyError) Error() string { return e.Message }

func orDefaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func nilIfEmptyString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// 上游错误里出现这些子串时，对外必须隐藏细节（可能含内部路径与状态）。
var internalErrorMarkers = []string{
	"backend-api/", "status=", "body=", "chatgpt.com", "upstreamhttperror",
}

// PublicImageErrorMessage 复刻 public_image_error_message：
// 含内部信息的错误统一替换成通用文案，避免泄漏上游细节。
func PublicImageErrorMessage(message string) string {
	text := strings.TrimSpace(message)
	lower := strings.ToLower(text)
	for _, marker := range internalErrorMarkers {
		if strings.Contains(lower, marker) {
			return "The image generation request failed. Please try again later."
		}
	}
	if text == "" {
		return "The image generation request failed. Please try again later."
	}
	return text
}

// IsTokenInvalidError 复刻 is_token_invalid_error：这类错误要换号。
func IsTokenInvalidError(message string) bool {
	text := strings.ToLower(message)
	for _, marker := range []string{
		"token_invalidated",
		"token_revoked",
		"authentication token has been invalidated",
		"invalidated oauth token",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// IsTLSConnectionError 复刻 is_tls_connection_error：这类错误可重试。
func IsTLSConnectionError(message string) bool {
	text := strings.ToLower(message)
	for _, marker := range []string{
		"curl: (35)",
		"tls connect error",
		"openssl_internal",
		"ssl: wrong_version_number",
		"ssl: certificate_verify_failed",
		"connection aborted",
		"remote disconnected",
		"connection reset by peer",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// IsConnectionTimeoutError 复刻 is_connection_timeout_error。
func IsConnectionTimeoutError(message string) bool {
	text := strings.ToLower(message)
	for _, marker := range []string{
		"curl: (28)",
		"operation timed out",
		"connection timed out",
		"read timed out",
		"connect timeout",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// ImageStreamErrorMessage 复刻 image_stream_error_message：按错误类型改写文案。
func ImageStreamErrorMessage(message string) string {
	if IsTokenInvalidError(message) {
		return "image generation failed"
	}
	if IsTLSConnectionError(message) {
		return "upstream image connection failed, please retry later"
	}
	if IsConnectionTimeoutError(message) {
		return "upstream connection timed out, please retry later"
	}
	if strings.TrimSpace(message) == "" {
		return "image generation failed"
	}
	return message
}

// referencedImageIDsRe 检测「模型返回了文本描述而非图片」的标记。
var referencedImageIDsRe = regexp.MustCompile(`"referenced_image_ids"\s*:\s*\[([^\]]+)\]`)

// IsModelTextReplyInsteadOfImage 复刻 is_model_text_reply_instead_of_image。
//
// 上游有时会返回一段文本（含 referenced_image_ids），说明它已发起图片工具调用
// 但 SSE 在工具完成前断开。这种要改用更积极的轮询，而不是把文本当结果。
func IsModelTextReplyInsteadOfImage(message string) bool {
	text := strings.TrimSpace(message)
	if text == "" {
		return false
	}
	return referencedImageIDsRe.MatchString(text)
}

// ImageOutputKind 是输出类型。
const (
	ImageKindProgress = "progress"
	ImageKindMessage  = "message"
	ImageKindResult   = "result"
)

// ImageOutput 复刻 ImageOutput 结构与其 to_chunk 的字段裁剪规则。
type ImageOutput struct {
	Kind              string
	Model             string
	Index             int
	Total             int
	Created           int64
	Text              string
	UpstreamEventType string
	Data              []map[string]any
	AccountEmail      string
	ConversationID    string
}

// ToChunk 复刻 ImageOutput.to_chunk。
//
// 字段裁剪规则是契约：
//   - progress：带 progress_text + upstream_event_type
//   - message：换成 object=image.generation.message + message 字段，删掉上面两个
//   - result：换成 object=image.generation.result + data 字段，同样删掉
func (o ImageOutput) ToChunk() map[string]any {
	chunk := map[string]any{
		"object":              "image.generation.chunk",
		"created":             o.Created,
		"model":               o.Model,
		"index":               o.Index,
		"total":               o.Total,
		"progress_text":       o.Text,
		"upstream_event_type": o.UpstreamEventType,
		"data":                []any{},
	}
	if o.AccountEmail != "" {
		chunk["_account_email"] = o.AccountEmail
	}
	if o.ConversationID != "" {
		chunk["_conversation_id"] = o.ConversationID
	}

	switch o.Kind {
	case ImageKindMessage:
		chunk["object"] = "image.generation.message"
		chunk["message"] = o.Text
		delete(chunk, "progress_text")
		delete(chunk, "upstream_event_type")
	case ImageKindResult:
		chunk["object"] = "image.generation.result"
		data := make([]any, 0, len(o.Data))
		for _, item := range o.Data {
			data = append(data, item)
		}
		chunk["data"] = data
		delete(chunk, "progress_text")
		delete(chunk, "upstream_event_type")
	}
	return chunk
}

// FormatImageResult 复刻 format_image_result。
//
// 注意 response_format=b64_json 时**同时**给 b64_json 与 url（图片会落盘）。
// 这是既有行为：客户端两种都能拿到。
//
// saveImage 的错误必须冒泡（Python 里 save_image_bytes 抛异常同样冒泡）：
// WebDAV 存储失败时整次生成要失败，而不是返回一个 url 为空的「成功」结果 ——
// 那会让 webdav 模式的用户以为图存好了。
func FormatImageResult(
	items []map[string]any,
	prompt, responseFormat string,
	saveImage func(payload []byte, baseURL string) (string, error),
	baseURL string,
	created int64,
	message string,
) (map[string]any, error) {
	data := make([]any, 0, len(items))
	for _, item := range items {
		encoded := strings.TrimSpace(stringOf(item["b64_json"]))
		if encoded == "" {
			continue
		}
		revisedPrompt := strings.TrimSpace(stringOf(item["revised_prompt"]))
		if revisedPrompt == "" {
			revisedPrompt = prompt
		}
		if strings.TrimSpace(revisedPrompt) == "" {
			revisedPrompt = prompt
		}
		payload, err := decodeBase64String(encoded)
		if err != nil {
			continue
		}
		url := ""
		if saveImage != nil {
			savedURL, saveErr := saveImage(payload, baseURL)
			if saveErr != nil {
				return nil, saveErr
			}
			url = savedURL
		}
		if responseFormat == "b64_json" {
			data = append(data, map[string]any{
				"b64_json":       encoded,
				"url":            url,
				"revised_prompt": revisedPrompt,
			})
			continue
		}
		data = append(data, map[string]any{
			"url":            url,
			"revised_prompt": revisedPrompt,
		})
	}

	result := map[string]any{"created": created, "data": data}
	if message != "" && len(data) == 0 {
		result["message"] = message
	}
	return result, nil
}

// ImageRequest 是一次图片生成的输入（对应 ConversationRequest 的图片相关字段）。
type ImageRequest struct {
	Prompt         string
	Model          string
	N              int
	Size           string
	Quality        string
	ResponseFormat string
	BaseURL        string
	Images         [][3]string
	MessageAsError bool
	// GlobalSystemPrompt 由 api 层从配置注入（protocol 不 import config）。
	GlobalSystemPrompt string
	// DefaultUpstreamModel / DefaultThinkingEffort 同上，供 gpt-image-2 映射用。
	DefaultUpstreamModel  string
	DefaultThinkingEffort string
	Progress              func(string)
}

// ErrNoImageQuota 与号池的 "no available image quota" 对应。
var ErrNoImageQuota = errors.New("no available image quota")

// NoImageQuotaError 构造带套餐前缀的配额错误文案（与 Python 一致）。
func NoImageQuotaError(label string) error {
	if strings.TrimSpace(label) == "" {
		return ErrNoImageQuota
	}
	return fmt.Errorf("no available %s image quota", strings.TrimSpace(label))
}
