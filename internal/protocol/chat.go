package protocol

import (
	"strings"
)

// Chat 协议常量。TOOL_UNAVAILABLE_SYSTEM_MESSAGE 的措辞是对外契约：
// 它会被注入到 system 消息里，改变模型行为。
const ToolUnavailableSystemMessage = "This compatibility backend cannot execute local tools, shell commands, non-search tools, " +
	"or file operations. Do not claim to have run tools or inspected external resources. " +
	"If a user asks you to use a tool, say that tool execution is unavailable through this backend."

// WebSearchToolTypes 是受支持的网页搜索工具类型。
var WebSearchToolTypes = map[string]bool{
	"web_search":                    true,
	"web_search_preview":            true,
	"web_search_preview_2025_03_11": true,
}

// SearchChatModelPrefixes 是按模型名触发搜索的模型前缀。
var SearchChatModelPrefixes = []string{
	"gpt-4o-search-preview",
	"gpt-4o-mini-search-preview",
	"gpt-5-search-api",
}

// NormalizeThinkingEffortForChat 复刻 openai_v1_chat_complete.normalize_thinking_effort。
//
// 注意与 NormalizeThinkingEffort（conversation payload 用的那个）**不同**：
// 这个接受 "none" 并返回 ""，且把 "xhigh" 映射成 "extended"。
func NormalizeThinkingEffortForChat(value any) string {
	normalized := strings.ToLower(strings.TrimSpace(stringOf(value)))
	switch normalized {
	case "", "none":
		return ""
	case "low", "medium", "high":
		return normalized
	case "xhigh", "extended":
		return "extended"
	default:
		return ""
	}
}

// ThinkingEffortFromBody 复刻 thinking_effort_from_body：
// 依次看 thinking_effort / reasoning_effort / reasoning.effort。
func ThinkingEffortFromBody(body map[string]any) string {
	if value, ok := body["thinking_effort"]; ok {
		return NormalizeThinkingEffortForChat(value)
	}
	if value, ok := body["reasoning_effort"]; ok {
		return NormalizeThinkingEffortForChat(value)
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		return NormalizeThinkingEffortForChat(reasoning["effort"])
	}
	return ""
}

// toolType 取工具的类型字段。
func toolType(tool any) string {
	entry, ok := tool.(map[string]any)
	if !ok {
		return ""
	}
	return strings.TrimSpace(stringOf(entry["type"]))
}

// HasWebSearchTool 复刻 has_web_search_tool。
func HasWebSearchTool(body map[string]any) bool {
	if tools, ok := body["tools"].([]any); ok {
		for _, tool := range tools {
			if WebSearchToolTypes[toolType(tool)] {
				return true
			}
		}
		return false
	}
	return WebSearchToolTypes[toolType(body["tool_choice"])]
}

// IsWebSearchChatRequest 复刻 is_web_search_chat_request。
func IsWebSearchChatRequest(body map[string]any) bool {
	if HasWebSearchTool(body) {
		return true
	}
	if _, ok := body["web_search_options"].(map[string]any); ok {
		return true
	}
	model := strings.TrimSpace(stringOf(body["model"]))
	for _, prefix := range SearchChatModelPrefixes {
		if model == prefix || strings.HasPrefix(model, prefix+"-") {
			return true
		}
	}
	return false
}

// HasUnsupportedTools 复刻 has_unsupported_tools：
// tools 里只要有一个不在 allowedTypes 里就算。
func HasUnsupportedTools(body map[string]any, allowedTypes map[string]bool) bool {
	tools, ok := body["tools"].([]any)
	if !ok {
		return false
	}
	for _, tool := range tools {
		if _, isMap := tool.(map[string]any); !isMap {
			continue
		}
		if !allowedTypes[toolType(tool)] {
			return true
		}
	}
	return false
}

// ChatMessagesFromBody 复刻 chat_messages_from_body：
// 优先 messages，回退到 prompt，都没有则报错。
func ChatMessagesFromBody(body map[string]any) ([]map[string]any, error) {
	if messages, ok := body["messages"].([]any); ok && len(messages) > 0 {
		out := make([]map[string]any, 0, len(messages))
		for _, item := range messages {
			if message, ok := item.(map[string]any); ok {
				out = append(out, message)
			}
		}
		return out, nil
	}
	prompt := strings.TrimSpace(stringOf(body["prompt"]))
	if prompt != "" {
		return []map[string]any{{"role": "user", "content": prompt}}, nil
	}
	return nil, ErrMissingMessages
}

// ErrMissingMessages 对应 Python 的 400 "messages or prompt is required"。
var ErrMissingMessages = errorString("messages or prompt is required")

type errorString string

func (e errorString) Error() string { return string(e) }

// ChatCompletionChunk 复刻 completion_chunk 的结构。
func ChatCompletionChunk(model string, delta map[string]any, finishReason any, completionID string, created int64) map[string]any {
	return map[string]any{
		"id":      completionID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
}

// ChatCompletionResponse 复刻 completion_response。
//
// promptTokens / completionTokens 由调用方算好后传入（Python 版在函数内用
// tiktoken 算，Go 侧把计数抽出去，便于测试与复用）。
func ChatCompletionResponse(
	model, content string,
	created int64,
	promptTextTokens, promptImageTokens, completionTokens int,
	annotations []any,
) map[string]any {
	promptTokens := promptTextTokens + promptImageTokens
	message := map[string]any{"role": "assistant", "content": content}
	if len(annotations) > 0 {
		message["annotations"] = annotations
	}
	return map[string]any{
		"id":      "chatcmpl-" + newUUIDValue(),
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
			"prompt_tokens_details": map[string]any{
				"text_tokens":   promptTextTokens,
				"image_tokens":  promptImageTokens,
				"cached_tokens": 0,
			},
			"completion_tokens_details": map[string]any{
				"text_tokens":      completionTokens,
				"image_tokens":     0,
				"reasoning_tokens": 0,
			},
		},
	}
}

// ChatCompletionAnnotations 复刻 chat_completion_annotations：
// 只保留 url_citation，并重排字段。
func ChatCompletionAnnotations(annotations []map[string]any) []any {
	out := make([]any, 0, len(annotations))
	for _, item := range annotations {
		if stringOf(item["type"]) != "url_citation" {
			continue
		}
		out = append(out, map[string]any{
			"type": "url_citation",
			"url_citation": map[string]any{
				"start_index": item["start_index"],
				"end_index":   item["end_index"],
				"url":         stringOf(item["url"]),
				"title":       stringOf(item["title"]),
			},
		})
	}
	return out
}
