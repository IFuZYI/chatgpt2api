package protocol

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// TextDeltaFunc 产出一段文本增量。返回错误会中止流。
type TextDeltaFunc func(ctx context.Context, onDelta func(string)) error

// StreamTextChatCompletion 复刻 stream_text_chat_completion：
// 逐段产出 chat.completion.chunk，首块带 role，末块带 finish_reason=stop。
//
// 即使上游一个字都没出，也必须发一个带 role 的空块 + 结束块，
// 否则客户端会一直等首块。
func StreamTextChatCompletion(
	model string,
	completionID string,
	created int64,
	produce TextDeltaFunc,
) ([]map[string]any, error) {
	chunks := make([]map[string]any, 0, 32)
	sentRole := false

	err := produce(context.Background(), func(delta string) {
		if !sentRole {
			sentRole = true
			chunks = append(chunks, ChatCompletionChunk(model,
				map[string]any{"role": "assistant", "content": delta}, nil, completionID, created))
			return
		}
		chunks = append(chunks, ChatCompletionChunk(model,
			map[string]any{"content": delta}, nil, completionID, created))
	})
	if err != nil {
		return chunks, err
	}
	if !sentRole {
		chunks = append(chunks, ChatCompletionChunk(model,
			map[string]any{"role": "assistant", "content": ""}, nil, completionID, created))
	}
	chunks = append(chunks, ChatCompletionChunk(model, map[string]any{}, "stop", completionID, created))
	return chunks, nil
}

// CollectChatContent 复刻 collect_chat_content：从 chunk 序列里拼出完整文本。
func CollectChatContent(chunks []map[string]any) string {
	var builder strings.Builder
	for _, chunk := range chunks {
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		first, _ := choices[0].(map[string]any)
		delta, _ := first["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok && content != "" {
			builder.WriteString(content)
		}
	}
	return builder.String()
}

// TextChatParts 复刻 text_chat_parts：
// 归一化消息，并在有不受支持的工具时注入 TOOL_UNAVAILABLE 系统提示。
func TextChatParts(body map[string]any, settings CacheSettings) (model string, messages []map[string]any, err error) {
	model = strings.TrimSpace(stringOf(body["model"]))
	if model == "" {
		model = "auto"
	}
	raw, err := ChatMessagesFromBody(body)
	if err != nil {
		return "", nil, err
	}
	normalized := make([]map[string]any, 0, len(raw))
	for _, message := range raw {
		normalized = append(normalized, normalizeMessageContent(message))
	}
	messages = NormalizeTextMessages(normalized, settings)

	if HasUnsupportedTools(body, WebSearchToolTypes) {
		messages = append([]map[string]any{
			{"role": "system", "content": ToolUnavailableSystemMessage},
		}, messages...)
	}
	return model, messages, nil
}

// normalizeMessageContent 复刻 conversation.message_text 的归一化：
// 把 list content 里的文本片段拼成字符串。
func normalizeMessageContent(message map[string]any) map[string]any {
	out := copyMapAny(message)
	out["content"] = MessageText(message["content"])
	return out
}

// MessageText 复刻 message_text：把各种 content 形态压成纯文本。
func MessageText(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, item := range typed {
			switch part := item.(type) {
			case string:
				builder.WriteString(part)
			case map[string]any:
				switch stringOf(part["type"]) {
				case "text", "input_text", "output_text":
					builder.WriteString(stringOf(part["text"]))
				}
			}
		}
		return builder.String()
	default:
		return ""
	}
}

func copyMapAny(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// ConversationTextRequest 是一次文本对话的输入。
type ConversationTextRequest struct {
	Model          string
	Messages       []map[string]any
	ThinkingEffort string
	// History 是历史 assistant 回复的文本序列。
	//
	// 上游会把整个对话历史（含此前的 assistant 回复）重新推一遍。
	// 不跳过这些历史片段的话，每一轮的输出都会以「上一轮的答案」开头
	// ——实测：历史 "previous answer" + 新答 "NEW" → 输出 "previous answerNEW"，
	// 而 Python 只输出 "NEW"。
	History []string
}

// StreamTextDeltas 复刻 stream_text_deltas 的核心循环（不含换号重试）。
//
// 换号重试由 api 层持有号池，所以这里只负责「一次对话 → 文本增量」。
func StreamTextDeltas(
	ctx context.Context,
	upstream Upstream,
	request ConversationTextRequest,
	onDelta func(string),
) error {
	payloads, errs, err := StreamConversation(ctx, upstream, request.Messages, request.Model, request.ThinkingEffort)
	if err != nil {
		return err
	}

	state := &ConversationState{}
	historyIndex := 0
	historyText := joinHistoryText(request.History)
	for payload := range payloads {
		if payload == "[DONE]" {
			break
		}
		var event map[string]any
		if !decodeObject(payload, &event) {
			UpdateConversationState(state, payload, nil)
			continue
		}
		UpdateConversationState(state, payload, event)

		nextRaw := AssistantRawText(event, state.RawText, historyText)

		// 跳过上游重放的历史片段（Python conversation.py:667-670）。
		if historyIndex < len(request.History) &&
			AssistantEventText(event, historyText) == request.History[historyIndex] {
			historyIndex++
			state.RawText = ""
			state.Text = ""
			continue
		}

		nextText := SanitizeOutputText(nextRaw)
		state.RawText = nextRaw
		if nextText == state.Text {
			continue
		}
		delta := nextText
		if strings.HasPrefix(nextText, state.Text) {
			delta = nextText[len(state.Text):]
		}
		state.Text = nextText
		if delta != "" {
			onDelta(delta)
		}
	}
	for streamErr := range errs {
		if streamErr != nil {
			return streamErr
		}
	}
	if state.Blocked {
		return fmt.Errorf("request was blocked by upstream moderation")
	}
	return nil
}

// CompletionID 生成 chatcmpl- 前缀的 id。
func CompletionID() string { return "chatcmpl-" + newUUIDValue() }

// NowUnix 返回当前 unix 秒（测试可替换）。
func NowUnix() int64 { return time.Now().Unix() }

// joinHistoryText 把历史 assistant 文本拼成上游会重放的整段前缀。
//
// Python 用 "\n\n".join(history_messages)（conversation.py:664）。
func joinHistoryText(history []string) string {
	if len(history) == 0 {
		return ""
	}
	filtered := make([]string, 0, len(history))
	for _, item := range history {
		if item != "" {
			filtered = append(filtered, item)
		}
	}
	// 用空串拼接：Python 是 `"".join(...)`（conversation.py:210）。
	//
	// 不能用 "\n\n"：真实输出里两段 assistant 回答之间没有分隔符，
	// 用带分隔符的前缀去 StripHistory 会找不到，导致历史不被剥离、
	// 客户端看到 "上一轮回答" 粘在 "本轮回答" 前面。
	return strings.Join(filtered, "")
}

// AssistantEventText 复刻 Python 的 event_assistant_text：
// 只取本事件里 assistant 可见消息的文本（不叠加 patch 状态）。
//
// 与 AssistantRawText 的区别：这里用于判断「这个事件是否正好是某段历史」，
// 所以必须是事件自身的完整文本，而不是增量拼接的结果。
func AssistantEventText(event map[string]any, historyText string) string {
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
	return ""
}

// HistoryTexts 从消息序列里提取历史 assistant 回复的文本。
//
// 上游会把它们整段重放回来，所以调用方必须把它们传给
// ConversationTextRequest.History，否则本轮输出会以历史内容开头。
func HistoryTexts(messages []map[string]any) []string {
	out := make([]string, 0, len(messages))
	for _, message := range messages {
		if stringOf(message["role"]) != "assistant" {
			continue
		}
		text := strings.TrimSpace(messageText(message["content"]))
		if text != "" {
			out = append(out, text)
		}
	}
	return out
}

// messageText 把 content（字符串或块数组）压成纯文本。
func messageText(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text := stringOf(entry["text"]); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}
