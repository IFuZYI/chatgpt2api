package protocol

import (
	"errors"
	"fmt"
	"strings"
)

// 本文件实现 /v1/responses 的「图片生成工具」形态：请求里带
// tools: [{"type": "image_generation"}]，走生图链路，输出 Responses 格式的事件。
//
// Python 权威实现：
//   - utils/helper.py:264-290                       extract_response_prompt
//   - utils/helper.py:292-298                       has_response_image_generation_tool
//   - services/protocol/openai_v1_response.py:78-122  response_image_tool /
//     extract_response_image / _input_image_parts
//   - services/protocol/openai_v1_response.py:193-245 text_output_item /
//     image_output_items
//   - services/protocol/openai_v1_response.py:248-410 response_created /
//     response_completed / stream_image_response / collect_response
//   - services/protocol/openai_v1_response.py:413-446 response_events 的图片分支
//
// 事件序列逐字对齐 Python：
//
//	response.created
//	├─ 文本回复：response.output_text.delta → response.output_text.done
//	│            → response.output_item.done → response.completed
//	└─ 图片结果：N × response.output_item.done（output_index 0..N-1）
//	             → response.completed
//
// ⚠️ 图片分支**没有** response.output_item.added，也没有
// response.image_generation_call.partial_image —— Python 的 stream_image_response
// 不产出这两个事件（有它们的只有文本与 web_search 分支）。多发明事件会让
// 客户端看到 Python 侧不存在的东西，属于发散，不要「补」上。

// ErrImageResponseGenerationFailed 对应 Python 的 RuntimeError("image generation failed")：
// 整条 output 流既没给出图片、也没给出文本回复。
var ErrImageResponseGenerationFailed = errors.New("image generation failed")

// ErrResponseCollectFailed 对应 collect_response 的 RuntimeError("response generation failed")。
var ErrResponseCollectFailed = errors.New("response generation failed")

// ResponseImage 是一张从 input 里解出来的参考图
// （Python extract_response_image 返回的 (bytes, mime) 二元组）。
type ResponseImage struct {
	Data     []byte
	MIMEType string
}

// HasResponseImageGenerationTool 复刻 has_response_image_generation_tool。
//
// 除了 tools 里显式的 image_generation，tool_choice 里的同类型选择同样算
// ——Python 两条都看，而 is_text_response_request 正是它的取反，漏掉
// tool_choice 会把图片请求误判成文本请求。
func HasResponseImageGenerationTool(body map[string]any) bool {
	if tools, ok := body["tools"].([]any); ok {
		for _, tool := range tools {
			entry, ok := tool.(map[string]any)
			if !ok {
				continue
			}
			if strings.TrimSpace(stringOf(entry["type"])) == "image_generation" {
				return true
			}
		}
	}
	toolChoice, ok := body["tool_choice"].(map[string]any)
	if !ok {
		return false
	}
	return strings.TrimSpace(stringOf(toolChoice["type"])) == "image_generation"
}

// ResponseImageTool 复刻 response_image_tool：返回第一个 image_generation 工具。
//
// 没有时返回 nil（Python 返回空 dict）：调用方直接取 size/quality 会得到 nil，
// 与 Python 的 tool.get(...) 同名同义。
func ResponseImageTool(body map[string]any) map[string]any {
	tools, ok := body["tools"].([]any)
	if !ok {
		return nil
	}
	for _, tool := range tools {
		entry, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(stringOf(entry["type"])) == "image_generation" {
			return entry
		}
	}
	return nil
}

// ExtractResponsePrompt 复刻 extract_response_prompt。
//
// 三种形态：
//   - 字符串：直接 strip
//   - 消息对象：非 user 角色返回空，否则取 content 里的文本
//   - 内容块数组 / 消息数组：input_text 块单独收，其余按 user 角色取文本，
//     用 "\n" 拼接后再 strip
func ExtractResponsePrompt(inputValue any) string {
	switch typed := inputValue.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		role := strings.ToLower(strings.TrimSpace(stringOf(typed["role"])))
		if role != "" && role != "user" {
			return ""
		}
		return extractPromptFromMessageContent(typed["content"])
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// input_text 块优先且独占一条分支：Python 命中后直接 continue，
			// 不再走下面的 role 过滤（否则 type=input_text 又没有 role 的块
			// 会被当消息处理一遍）。
			if strings.TrimSpace(stringOf(entry["type"])) == "input_text" {
				if text := strings.TrimSpace(stringOf(entry["text"])); text != "" {
					parts = append(parts, text)
				}
				continue
			}
			role := strings.ToLower(strings.TrimSpace(stringOf(entry["role"])))
			if role != "" && role != "user" {
				continue
			}
			if prompt := extractPromptFromMessageContent(entry["content"]); prompt != "" {
				parts = append(parts, prompt)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

// ExtractResponseImage 复刻 extract_response_image。
//
// 返回 (nil, nil) 表示没有参考图。逆序遍历数组（后出现的图优先），
// 并且**同一个 item 上两处都要看**：先按 item 自身的 input_image 解，
// 再按 item.content 里的块解（Python 是连续两个 if，不是 elif）。
func ExtractResponseImage(inputValue any) (*ResponseImage, error) {
	switch typed := inputValue.(type) {
	case map[string]any:
		if strings.TrimSpace(stringOf(typed["type"])) == "input_image" {
			return firstResponseImage([]any{typed})
		}
		return firstResponseImage(typed["content"])
	case []any:
		for index := len(typed) - 1; index >= 0; index-- {
			entry, ok := typed[index].(map[string]any)
			if !ok {
				continue
			}
			if strings.TrimSpace(stringOf(entry["type"])) == "input_image" {
				image, err := firstResponseImage([]any{entry})
				if err != nil || image != nil {
					return image, err
				}
			}
			image, err := firstResponseImage(entry["content"])
			if err != nil || image != nil {
				return image, err
			}
		}
		return nil, nil
	default:
		return nil, nil
	}
}

// firstResponseImage 取 content 里的第一张图（Python 的 images[0] if images else None）。
func firstResponseImage(content any) (*ResponseImage, error) {
	images, err := extractImagesFromMessageContent(content)
	if err != nil || len(images) == 0 {
		return nil, err
	}
	return &ResponseImage{Data: images[0].data, MIMEType: images[0].mime}, nil
}

// CountResponseImageInputTokens 复刻
// count_image_content_tokens(_input_image_parts(input), model)。
//
// 与 usage 里的 input_image_tokens 是同一个数：Python 就是这么算的，
// 只统计 input 里真正带图的部分。
func CountResponseImageInputTokens(inputValue any, model string) int {
	return countImageContentTokens(responseInputImageParts(inputValue), model)
}

// responseInputImageParts 复刻 _input_image_parts。
//
// 「全部都是内容块」时（每项都有 type）直接把它们当图片部分；否则按消息
// 展开，取每条消息 content 数组里的块。空数组视为全部是内容块（Python 的
// all([]) 为 True），结果同样是空。
func responseInputImageParts(inputValue any) []any {
	parts := make([]any, 0, 4)
	switch typed := inputValue.(type) {
	case map[string]any:
		content, ok := typed["content"].([]any)
		if !ok {
			return parts
		}
		for _, item := range content {
			if entry, ok := item.(map[string]any); ok {
				parts = append(parts, entry)
			}
		}
		return parts
	case []any:
		allParts := true
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok || strings.TrimSpace(stringOf(entry["type"])) == "" {
				allParts = false
				break
			}
		}
		if allParts {
			for _, item := range typed {
				if entry, ok := item.(map[string]any); ok {
					parts = append(parts, entry)
				}
			}
			return parts
		}
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			content, ok := entry["content"].([]any)
			if !ok {
				continue
			}
			for _, part := range content {
				if object, ok := part.(map[string]any); ok {
					parts = append(parts, object)
				}
			}
		}
		return parts
	default:
		return parts
	}
}

// ImageOutputItems 复刻 image_output_items。
//
// 只保留带 b64_json 的条目，序号按**已产出条数**递增（跳过空条目后仍然连续，
// 与 Python 的 len(output) + 1 一致）。
//
// revised_prompt 严格复刻 `str(item.get("revised_prompt") or prompt).strip() or prompt`：
// 条目里有非空的 revised_prompt 就用它（strip 后为空则回落 prompt），否则用
// strip 后的 prompt（同样为空则用原始 prompt）。
//
// Python 的 item_id 参数在 Python 侧也没有调用方传值，Go 侧固定生成 ig_N
// —— 与 Python 的默认分支完全一致。
func ImageOutputItems(prompt string, data []map[string]any) []map[string]any {
	output := make([]map[string]any, 0, len(data))
	for _, item := range data {
		encoded := strings.TrimSpace(stringOf(item["b64_json"]))
		if encoded == "" {
			continue
		}
		revisedPrompt := strings.TrimSpace(stringOf(chatValueOr(item["revised_prompt"], prompt)))
		if revisedPrompt == "" {
			revisedPrompt = prompt
		}
		output = append(output, map[string]any{
			"id":             fmt.Sprintf("ig_%d", len(output)+1),
			"type":           "image_generation_call",
			"status":         "completed",
			"result":         encoded,
			"revised_prompt": revisedPrompt,
		})
	}
	return output
}

// textResponseOutputItem 复刻 text_output_item 的图片分支用法（text_output_item(text)）。
//
// 与文本链路不同：这里的 status 固定 completed，没有 in_progress 版本
// ——Python 的图片分支不发 output_item.added。
func textResponseOutputItem(text string) map[string]any {
	return map[string]any{
		"id":     "msg_" + newUUIDHex(),
		"type":   "message",
		"status": "completed",
		"role":   "assistant",
		"content": []any{map[string]any{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		}},
	}
}

// responseCreatedEvent 复刻 response_created。
func responseCreatedEvent(responseID, model string, created int64) map[string]any {
	return map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":                  responseID,
			"object":              "response",
			"created_at":          created,
			"status":              "in_progress",
			"error":               nil,
			"incomplete_details":  nil,
			"model":               model,
			"output":              []any{},
			"parallel_tool_calls": false,
		},
	}
}

// responseCompletedEvent 复刻 response_completed（usage 非空才带）。
func responseCompletedEvent(responseID, model string, created int64, output []any, usage map[string]any) map[string]any {
	response := map[string]any{
		"id":                  responseID,
		"object":              "response",
		"created_at":          created,
		"status":              "completed",
		"error":               nil,
		"incomplete_details":  nil,
		"model":               model,
		"output":              output,
		"parallel_tool_calls": false,
	}
	if len(usage) > 0 {
		response["usage"] = usage
	}
	return map[string]any{"type": "response.completed", "response": response}
}

// StreamImageResponse 复刻 stream_image_response。
//
// outputs 按到达顺序给出；emit 逐条写出事件（SSE 或收集到切片）。
// emit 返回 nil 表示继续，非 nil 表示写客户端失败，立即停止消费并把它原样返回。
//
// 语义逐条对齐 Python：
//   - 首条一定是 response.created（在读到任何 output 之前发出）
//   - kind=message：delta → done → output_item.done → completed，然后结束
//     （即使文本为空也照发，Python 没有 content 判断）
//   - kind=result：每条 data 一个 output_item.done，再 completed
//     （一条图都没有时**不结束**，继续看后面的 output）
//   - 其余 kind（progress）：跳过
//   - 流走完还没有可用输出 → ErrImageResponseGenerationFailed
func StreamImageResponse(
	outputs <-chan ImageOutput,
	prompt, model string,
	inputImageTokens int,
	size any,
	quality string,
	emit func(event map[string]any) error,
) error {
	responseID := "resp_" + newUUIDHex()
	created := NowUnix()
	if err := emit(responseCreatedEvent(responseID, model, created)); err != nil {
		return err
	}

	for output := range outputs {
		switch output.Kind {
		case ImageKindMessage:
			text := output.Text
			item := textResponseOutputItem(text)
			usage := TokenUsage(
				CountTextTokens(prompt, model),
				inputImageTokens,
				CountTextTokens(text, model),
				0,
			)
			events := []map[string]any{
				{
					"type": "response.output_text.delta", "item_id": item["id"],
					"output_index": 0, "content_index": 0, "delta": text,
				},
				{
					"type": "response.output_text.done", "item_id": item["id"],
					"output_index": 0, "content_index": 0, "text": text,
				},
				{"type": "response.output_item.done", "output_index": 0, "item": item},
				responseCompletedEvent(responseID, model, created, []any{item}, usage),
			}
			for _, event := range events {
				if err := emit(event); err != nil {
					return err
				}
			}
			return nil

		case ImageKindResult:
			items := ImageOutputItems(prompt, output.Data)
			if len(items) == 0 {
				// 这个 output 没有可用图：Python 是 continue，继续等后面的。
				continue
			}
			usage := ImageUsage(
				CountTextTokens(prompt, model),
				inputImageTokens,
				CountImageOutputItemsTokens(output.Data, size, quality),
			)
			completed := make([]any, 0, len(items))
			for index, item := range items {
				completed = append(completed, item)
				if err := emit(map[string]any{
					"type": "response.output_item.done", "output_index": index, "item": item,
				}); err != nil {
					return err
				}
			}
			return emit(responseCompletedEvent(responseID, model, created, completed, usage))
		}
	}
	return ErrImageResponseGenerationFailed
}

// CollectResponse 复刻 collect_response：从事件序列里取 response.completed
// 的 response 对象（非流式响应就是它）。
func CollectResponse(events []map[string]any) (map[string]any, error) {
	var completed map[string]any
	for _, event := range events {
		if stringOf(event["type"]) != "response.completed" {
			continue
		}
		if response, ok := event["response"].(map[string]any); ok {
			completed = response
		}
	}
	if len(completed) == 0 {
		return nil, ErrResponseCollectFailed
	}
	return completed, nil
}
