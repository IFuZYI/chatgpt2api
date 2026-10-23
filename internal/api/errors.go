package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Envelope 三选一：管理面 / OpenAI 兼容 / Anthropic。见 ADR-0005。
type Envelope int

const (
	EnvelopeAdmin Envelope = iota
	EnvelopeOpenAI
	EnvelopeAnthropic
)

// EnvelopeForPath 决定某个路径该用哪套错误信封。
func EnvelopeForPath(path string) Envelope {
	switch {
	case path == "/v1/messages":
		return EnvelopeAnthropic
	case path == "/v1" || strings.HasPrefix(path, "/v1/"):
		return EnvelopeOpenAI
	default:
		return EnvelopeAdmin
	}
}

// HTTPError 是 handler 内部抛出的错误，带状态码与原始 detail。
// detail 的形状决定最终信封内容，与 FastAPI 的 HTTPException(detail=...) 对应。
type HTTPError struct {
	Status int
	Detail any
	// Headers 用于少数需要带 header 的错误（如 405 的 Allow）。
	Headers map[string]string
}

func (e *HTTPError) Error() string {
	if text, ok := e.Detail.(string); ok {
		return text
	}
	if payload, err := json.Marshal(e.Detail); err == nil {
		return string(payload)
	}
	return http.StatusText(e.Status)
}

// Errorf 构造管理面最常见的 {"error": "..."} 形状。
func Errorf(status int, message string) *HTTPError {
	return &HTTPError{Status: status, Detail: map[string]any{"error": message}}
}

// ErrorDetail 允许传入任意 detail 结构（用于 422 的数组）。
func ErrorDetail(status int, detail any) *HTTPError {
	return &HTTPError{Status: status, Detail: detail}
}

// errorMessageFromDetail 复刻 Python 的 error_message_from_detail。
func errorMessageFromDetail(detail any) string {
	switch typed := detail.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		messages := make([]string, 0, len(typed))
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			location := joinLoc(entry["loc"])
			message := strings.TrimSpace(stringOf(entry["msg"]))
			switch {
			case location != "" && message != "":
				messages = append(messages, location+": "+message)
			case message != "":
				messages = append(messages, message)
			}
		}
		return strings.Join(messages, "; ")
	case map[string]any:
		if message := messageFromValue(typed["error"]); message != "" {
			return message
		}
		if message := messageFromValue(typed); message != "" {
			return message
		}
		return ""
	default:
		return strings.TrimSpace(stringOf(typed))
	}
}

func joinLoc(value any) string {
	items, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]string); ok {
			parts := make([]string, 0, len(typed))
			for _, part := range typed {
				if part != "body" {
					parts = append(parts, part)
				}
			}
			return strings.Join(parts, ".")
		}
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		text := stringOf(item)
		if text == "" || text == "body" {
			continue
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, ".")
}

func messageFromValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		if message, ok := typed["message"].(string); ok && message != "" {
			return message
		}
		return messageFromValue(typed["error"])
	default:
		return ""
	}
}

func stringOf(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func defaultErrorType(status int) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 429:
		return "rate_limit_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "server_error"
	}
}

func defaultErrorCode(status int) string {
	switch {
	case status == 401:
		return "invalid_api_key"
	case status == 403:
		return "permission_denied"
	case status == 429:
		return "rate_limit_exceeded"
	case status >= 400 && status < 500:
		return "bad_request"
	default:
		return "upstream_error"
	}
}

// openaiErrorPayload 复刻 services/protocol/error_response.py 的推导。
func openaiErrorPayload(detail any, status int) map[string]any {
	if wrapper, ok := detail.(map[string]any); ok {
		if inner, ok := wrapper["error"].(map[string]any); ok {
			message := errorMessageFromDetail(inner)
			if message == "" {
				message = "request failed"
			}
			errorType := stringOf(inner["type"])
			if errorType == "" {
				errorType = defaultErrorType(status)
			}
			code := inner["code"]
			if code == nil {
				code = defaultErrorCode(status)
			}
			return map[string]any{
				"error": map[string]any{
					"message": message,
					"type":    errorType,
					"param":   inner["param"],
					"code":    code,
				},
			}
		}
	}
	message := errorMessageFromDetail(detail)
	if message == "" {
		message = "request failed"
	}
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    defaultErrorType(status),
			"param":   nil,
			"code":    defaultErrorCode(status),
		},
	}
}

func anthropicErrorPayload(detail any, status int) map[string]any {
	errorType := "api_error"
	if status < 500 {
		errorType = defaultErrorType(status)
	}
	message := errorMessageFromDetail(detail)
	if message == "" {
		message = "request failed"
	}
	return map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errorType,
			"message": message,
		},
	}
}

// WriteError 按路径选择信封写出错误响应。
func WriteError(c *gin.Context, err *HTTPError) {
	for key, value := range err.Headers {
		c.Header(key, value)
	}
	envelope := EnvelopeForPath(c.Request.URL.Path)
	switch envelope {
	case EnvelopeOpenAI:
		c.JSON(err.Status, openaiErrorPayload(err.Detail, err.Status))
	case EnvelopeAnthropic:
		c.JSON(err.Status, anthropicErrorPayload(err.Detail, err.Status))
	default:
		c.JSON(err.Status, map[string]any{"detail": err.Detail})
	}
}

// WriteErrorEnvelope 允许调用方强制指定信封（流式响应内部错误用）。
func WriteErrorEnvelope(c *gin.Context, err *HTTPError, envelope Envelope) {
	for key, value := range err.Headers {
		c.Header(key, value)
	}
	switch envelope {
	case EnvelopeOpenAI:
		c.JSON(err.Status, openaiErrorPayload(err.Detail, err.Status))
	case EnvelopeAnthropic:
		c.JSON(err.Status, anthropicErrorPayload(err.Detail, err.Status))
	default:
		c.JSON(err.Status, map[string]any{"detail": err.Detail})
	}
}

// StatusError 构造 FastAPI 框架级错误的形状：detail 是纯字符串。
// 与 Errorf 的区别是信封内容不同：
//
//	StatusError(404, "Not Found")   -> {"detail": "Not Found"}
//	Errorf(404, "Not Found")        -> {"detail": {"error": "Not Found"}}
//
// 前者来自 Starlette 默认的 HTTPException，后者来自业务代码显式抛的
// HTTPException(detail={"error": ...})。两者在 Python 版里同时存在，
// 必须都复刻（见 ADR-0005）。
func StatusError(status int, detail string) *HTTPError {
	return &HTTPError{Status: status, Detail: detail}
}
