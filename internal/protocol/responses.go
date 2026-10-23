package protocol

import "strings"

// MessagesFromInput 复刻 messages_from_input：
// input 支持字符串、单个消息对象、内容块数组、消息数组四种形态。
func MessagesFromInput(inputValue any, instructions any) []map[string]any {
	messages := make([]map[string]any, 0, 4)

	if systemText := strings.TrimSpace(stringOf(instructions)); systemText != "" {
		messages = append(messages, map[string]any{"role": "system", "content": systemText})
	}

	switch typed := inputValue.(type) {
	case string:
		if strings.TrimSpace(typed) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": strings.TrimSpace(typed)})
		}
		return messages

	case map[string]any:
		if isResponseContentPart(typed) {
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": []any{copyMapAny(typed)},
			})
			return messages
		}
		role := orDefaultValue(stringOf(typed["role"]), "user")
		messages = appendResponseMessage(messages, role, messageContentFromItem(typed))
		return messages

	case []any:
		// 全部是内容块：拼成一条 user 消息。
		allParts := len(typed) > 0
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok || !isResponseContentPart(entry) {
				allParts = false
				break
			}
		}
		if allParts {
			parts := make([]any, 0, len(typed))
			for _, item := range typed {
				if entry, ok := item.(map[string]any); ok {
					parts = append(parts, copyMapAny(entry))
				}
			}
			messages = appendResponseMessage(messages, "user", parts)
			return messages
		}

		// 混合：内容块归到前一条 user 消息，其余按 role 展开。
		pending := make([]any, 0, 2)
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if ok && isResponseContentPart(entry) {
				pending = append(pending, copyMapAny(entry))
				continue
			}
			if len(pending) > 0 {
				messages = appendResponseMessage(messages, "user", pending)
				pending = make([]any, 0, 2)
			}
			if !ok {
				continue
			}
			role := orDefaultValue(stringOf(entry["role"]), "user")
			messages = appendResponseMessage(messages, role, messageContentFromItem(entry))
		}
		if len(pending) > 0 {
			messages = appendResponseMessage(messages, "user", pending)
		}
		return messages

	default:
		return messages
	}
}

// isResponseContentPart 判断是否是内容块（input_text / input_image 等）。
func isResponseContentPart(value map[string]any) bool {
	switch stringOf(value["type"]) {
	case "input_text", "input_image", "text", "image_url", "image", "output_text":
		return true
	default:
		return false
	}
}

// messageContentFromItem 复刻 _message_content_from_response_item：
// 消息对象的 content 可能是字符串或块数组，两种都归一化成块数组。
func messageContentFromItem(item map[string]any) any {
	content := item["content"]
	switch typed := content.(type) {
	case string:
		return []any{map[string]any{"type": "input_text", "text": typed}}
	case []any:
		return typed
	case nil:
		return []any{}
	default:
		return []any{}
	}
}

func appendResponseMessage(messages []map[string]any, role string, content any) []map[string]any {
	return append(messages, map[string]any{"role": role, "content": content})
}

// ResponseObject 组装非流式的 response 对象。
//
// 字段集与 Python 的 response_completed 一致：id / object / created_at /
// status / error / incomplete_details / model / output / parallel_tool_calls / usage。
func ResponseObject(model, content string, messages []map[string]any) map[string]any {
	output := []any{map[string]any{
		"id":     "msg_" + newUUIDValue(),
		"type":   "message",
		"status": "completed",
		"role":   "assistant",
		"content": []any{map[string]any{
			"type":        "output_text",
			"text":        content,
			"annotations": []any{},
		}},
	}}

	return map[string]any{
		"id":                  "resp_" + newUUIDValue(),
		"object":              "response",
		"created_at":          NowUnix(),
		"status":              "completed",
		"error":               nil,
		"incomplete_details":  nil,
		"model":               model,
		"output":              output,
		"parallel_tool_calls": false,
		"usage": TokenUsage(
			CountMessageTextTokens(messages, model),
			0,
			CountTextTokens(content, model),
			0,
		),
	}
}

// ResponseStreamEvents 把完整 response 拆成事件序列（created + completed）。
//
// Python 的流式实现是逐 delta 产出 response.output_text.delta；
// 这里因为没有真实的增量来源（上层先收集完再返回），只发
// created + output_text.delta（一次性）+ completed，语义等价。
func ResponseStreamEvents(response map[string]any) []map[string]any {
	responseID := stringOf(response["id"])
	model := stringOf(response["model"])
	createdAt := int64(intValueOf(response["created_at"]))

	events := make([]map[string]any, 0, 4)
	events = append(events, map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": responseID, "object": "response", "created_at": createdAt,
			"status": "in_progress", "error": nil, "incomplete_details": nil,
			"model": model, "output": []any{}, "parallel_tool_calls": false,
		},
	})

	// 把文本作为一次 delta 发出（保持事件形状，客户端可正常渲染）。
	if output, ok := response["output"].([]any); ok && len(output) > 0 {
		if item, ok := output[0].(map[string]any); ok {
			if content, ok := item["content"].([]any); ok && len(content) > 0 {
				if part, ok := content[0].(map[string]any); ok {
					events = append(events, map[string]any{
						"type":    "response.output_text.delta",
						"item_id": item["id"],
						"delta":   part["text"],
					})
				}
			}
		}
	}

	events = append(events, map[string]any{
		"type":     "response.completed",
		"response": response,
	})
	return events
}
