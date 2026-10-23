package protocol

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
)

// Anthropic 协议适配层。复刻 services/protocol/anthropic_v1_messages.py。
//
// 核心职责：把 OpenAI 风格的文本输出「伪装」成 Anthropic Messages 协议。
// 关键难点是工具调用：上游只会输出文本，所以让模型按约定输出 XML，
// 再由这里解析成 tool_use 块。

// XMLToolRule 是注入给 Claude Code 系 system 的补充规则。
const XMLToolRule = `Tool output adapter: when calling tools, output ONLY this XML and no prose/markdown:
<tool_calls><tool_call><tool_name>TOOL_NAME</tool_name><parameters><PARAM><![CDATA[value]]></PARAM></parameters></tool_call></tool_calls>`

var (
	// stripToolMarkupRe 删除所有工具标记块。
	stripToolMarkupRe = regexp.MustCompile(`(?is)<tool_calls\b[^>]*>.*?</tool_calls>|<tool_call\b[^>]*>.*?</tool_call>|<function_call\b[^>]*>.*?</function_call>|<invoke\b[^>]*>.*?</invoke>`)
	// streamableTextRe 定位第一个工具标记的起点。
	streamableTextRe = regexp.MustCompile(`(?is)<tool_calls\b|<tool_call\b|<function_call\b|<invoke\b`)
	// codeFenceRe 删除 ``` 代码块（解析工具调用前先剔除）。
	codeFenceRe = regexp.MustCompile("(?is)```.*?```")
	// toolCallBlockRe 提取工具调用块的内容。
	toolCallBlockRe = regexp.MustCompile(`(?is)<tool_call\b[^>]*>(.*?)</tool_call>|<function_call\b[^>]*>(.*?)</function_call>|<invoke\b[^>]*>(.*?)</invoke>`)
	// openTagRe 匹配开标签（Go 的 regexp 不支持反向引用，所以参数解析要手动配对）。
	openTagRe = regexp.MustCompile(`(?is)<([\w.-]+)\b[^>]*>`)
	// cdataRe 识别 <![CDATA[...]]> 包裹。
	cdataRe = regexp.MustCompile(`(?is)^<!\[CDATA\[(.*?)]]>$`)
)

// ToolMeta 是工具的名字/描述/参数 schema。
type ToolMeta struct {
	Name        string
	Description string
	Schema      any
}

// toolMetaOf 复刻 _tool_meta：兼容 Anthropic 与 OpenAI 两种工具形状。
func toolMetaOf(tool map[string]any) ToolMeta {
	fn, _ := tool["function"].(map[string]any)
	pick := func(keys ...string) string {
		for _, key := range keys {
			if value := strings.TrimSpace(stringOf(tool[key])); value != "" {
				return value
			}
			if fn != nil {
				if value := strings.TrimSpace(stringOf(fn[key])); value != "" {
					return value
				}
			}
		}
		return ""
	}
	schema := tool["input_schema"]
	if schema == nil {
		schema = tool["parameters"]
	}
	if schema == nil && fn != nil {
		schema = fn["input_schema"]
		if schema == nil {
			schema = fn["parameters"]
		}
	}
	if schema == nil {
		schema = map[string]any{}
	}
	return ToolMeta{
		Name:        pick("name"),
		Description: pick("description"),
		Schema:      schema,
	}
}

// BuildToolPrompt 复刻 build_tool_prompt。
//
// ⚠️ 输出格式有两点「看起来像 bug」的地方，但都是 Python 的既有行为，
// 必须原样保留（实测核对过 repr）：
//
//  1. 多个工具的 block 用 "".join 拼接，**没有分隔符** —— 所以两个工具之间是
//     `...Parameters: {...}Tool: f2...` 直接黏在一起。
//  2. `Tool use rules:` 紧接在最后一个 block 后面，**没有换行**。
//
// 这两点会影响模型对提示的理解，改动等于改提示词，不能"顺手修好"。
func BuildToolPrompt(tools any) string {
	list, ok := tools.([]any)
	if !ok {
		return ""
	}
	var builder strings.Builder
	blocks := 0
	for _, item := range list {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		meta := toolMetaOf(tool)
		if meta.Name == "" {
			continue
		}
		schemaJSON, _ := json.Marshal(meta.Schema)
		builder.WriteString("Tool: " + meta.Name + "\nDescription: " + meta.Description +
			"\nParameters: " + string(schemaJSON))
		blocks++
	}
	if blocks == 0 {
		return ""
	}
	return "Available tools:\n" + builder.String() + toolUseRules
}

// toolUseRules 是 build_tool_prompt 的固定尾段（注意：前面没有换行）。
const toolUseRules = `Tool use rules:
- If the user asks to list/read/search files, inspect project state, run a command, or answer from local code, you MUST call a suitable tool first. Do not say you cannot access files.
- To call tools, output ONLY XML and no prose/markdown:
<tool_calls><tool_call><tool_name>TOOL_NAME</tool_name><parameters><PARAM><![CDATA[value]]></PARAM></parameters></tool_call></tool_calls>
- Put parameters under <parameters> using the exact schema names.`

// HasClaudeCodeSystem 复刻 _has_claude_code_system。
func HasClaudeCodeSystem(system any) bool {
	switch typed := system.(type) {
	case string:
		return strings.Contains(typed, "You are Claude Code")
	case []any:
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if ok && strings.Contains(stringOf(entry["text"]), "You are Claude Code") {
				return true
			}
		}
	}
	return false
}

// MergeSystem 复刻 merge_system：把工具提示合并进 system。
func MergeSystem(system any, extra string) any {
	if HasClaudeCodeSystem(system) {
		extra = XMLToolRule
	}
	if extra == "" {
		return system
	}
	switch typed := system.(type) {
	case string:
		if strings.TrimSpace(typed) != "" {
			return strings.TrimSpace(typed) + "\n\n" + extra
		}
		return extra
	case []any:
		return append(typed, map[string]any{"type": "text", "text": extra})
	default:
		return extra
	}
}

// PreprocessBlock 复刻 _preprocess_block：
// tool_use / tool_result 块要转成文本，因为上游不认识它们。
func PreprocessBlock(block any) any {
	entry, ok := block.(map[string]any)
	if !ok {
		return block
	}
	switch stringOf(entry["type"]) {
	case "text":
		return map[string]any{"type": "text", "text": stringOf(entry["text"])}
	case "tool_use":
		params, _ := json.Marshal(entry["input"])
		if entry["input"] == nil {
			params = []byte("{}")
		}
		return map[string]any{
			"type": "text",
			"text": "<tool_calls><tool_call><tool_name>" + stringOf(entry["name"]) +
				"</tool_name><parameters>" + string(params) + "</parameters></tool_call></tool_calls>",
		}
	case "tool_result":
		return map[string]any{
			"type": "text",
			"text": "Tool result " + stringOf(entry["tool_use_id"]) + ": " + stringOf(entry["content"]),
		}
	default:
		return block
	}
}

// PreprocessMessages 复刻 preprocess_messages。
func PreprocessMessages(messages any) any {
	list, ok := messages.([]any)
	if !ok {
		return messages
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		copied := copyMapAny(message)
		switch content := copied["content"].(type) {
		case string:
			copied["content"] = content
		case []any:
			blocks := make([]any, 0, len(content))
			for _, block := range content {
				blocks = append(blocks, PreprocessBlock(block))
			}
			copied["content"] = blocks
		}
		out = append(out, copied)
	}
	return out
}

// PreprocessPayload 复刻 preprocess_payload。
func PreprocessPayload(body map[string]any) map[string]any {
	out := copyMapAny(body)
	out["messages"] = PreprocessMessages(out["messages"])
	out["system"] = MergeSystem(out["system"], BuildToolPrompt(out["tools"]))
	return out
}

// StripToolMarkup 复刻 strip_tool_markup。
func StripToolMarkup(text string) string {
	return strings.TrimSpace(stripToolMarkupRe.ReplaceAllString(text, ""))
}

// StreamableText 复刻 streamable_text：截到第一个工具标记之前。
func StreamableText(text string) string {
	loc := streamableTextRe.FindStringIndex(text)
	if loc == nil {
		return text
	}
	return strings.TrimRight(text[:loc[0]], " \t\r\n")
}

// XMLValue 复刻 xml_value：取标签内容并解 CDATA / HTML 实体。
func XMLValue(text, tag string) string {
	pattern, err := regexp.Compile(`(?is)<` + regexp.QuoteMeta(tag) + `\b[^>]*>(.*?)</` + regexp.QuoteMeta(tag) + `>`)
	if err != nil {
		return ""
	}
	match := pattern.FindStringSubmatch(text)
	if len(match) < 2 {
		return ""
	}
	value := strings.TrimSpace(match[1])
	if cdata := cdataRe.FindStringSubmatch(value); len(cdata) > 1 {
		return strings.TrimSpace(html.UnescapeString(cdata[1]))
	}
	return strings.TrimSpace(html.UnescapeString(value))
}

// ParseToolValue 复刻 parse_tool_value：先当 JSON，失败则原样返回。
func ParseToolValue(raw string) any {
	value := XMLValue("<x>"+raw+"</x>", "x")
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err == nil {
		return decoded
	}
	return value
}

// ParseToolParams 复刻 parse_tool_params：先当 JSON 对象，失败则按标签解析。
func ParseToolParams(raw string) map[string]any {
	trimmed := strings.TrimSpace(raw)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil && decoded != nil {
		return decoded
	}
	return parseTagPairs(trimmed)
}

// parseTagPairs 手动配对 <tag>value</tag>。
//
// Go 的 regexp 不支持反向引用，Python 的 `</\1>` 写法没法直接翻译。
// 这里按开标签逐个扫描，找同名闭标签。
func parseTagPairs(text string) map[string]any {
	out := map[string]any{}
	// FindAllStringSubmatchIndex 每组返回 [full_start, full_end, group_start, group_end]。
	matches := openTagRe.FindAllStringSubmatchIndex(text, -1)
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		name := text[match[2]:match[3]]
		contentStart := match[1]
		closeTag := "</" + name + ">"
		// 从内容起点找同名闭标签（大小写不敏感）。
		rest := strings.ToLower(text[contentStart:])
		offset := strings.Index(rest, strings.ToLower(closeTag))
		if offset < 0 {
			continue
		}
		value := text[contentStart : contentStart+offset]
		out[name] = ParseToolValue(value)
	}
	return out
}

// ParseToolCalls 复刻 parse_tool_calls，返回 (name, input) 序列。
func ParseToolCalls(text string) []struct {
	Name  string
	Input map[string]any
} {
	cleaned := strings.TrimSpace(codeFenceRe.ReplaceAllString(text, ""))
	out := make([]struct {
		Name  string
		Input map[string]any
	}, 0, 2)

	for _, match := range toolCallBlockRe.FindAllStringSubmatch(cleaned, -1) {
		block := ""
		for _, part := range match[1:] {
			if part != "" {
				block = part
				break
			}
		}
		if block == "" {
			continue
		}
		name := XMLValue(block, "tool_name")
		if name == "" {
			name = XMLValue(block, "name")
		}
		if name == "" {
			name = XMLValue(block, "function")
		}
		params := XMLValue(block, "parameters")
		if params == "" {
			params = XMLValue(block, "input")
		}
		if params == "" {
			params = XMLValue(block, "arguments")
		}
		if params == "" {
			params = "{}"
		}
		if name == "" {
			continue
		}
		out = append(out, struct {
			Name  string
			Input map[string]any
		}{Name: name, Input: ParseToolParams(params)})
	}
	return out
}

// ContentBlocks 复刻 content_blocks：把文本拆成 Anthropic content 块。
func ContentBlocks(text string, tools any) ([]map[string]any, string) {
	var calls []struct {
		Name  string
		Input map[string]any
	}
	if list, ok := tools.([]any); ok && len(list) > 0 {
		calls = ParseToolCalls(text)
	}
	stripped := StripToolMarkup(text)
	if len(calls) > 0 {
		content := make([]map[string]any, 0, len(calls)+1)
		if stripped != "" {
			content = append(content, map[string]any{"type": "text", "text": stripped})
		}
		for _, call := range calls {
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    "toolu_" + newUUIDValue(),
				"name":  call.Name,
				"input": call.Input,
			})
		}
		return content, "tool_use"
	}
	return []map[string]any{{"type": "text", "text": stripped}}, "end_turn"
}

// MessageResponse 复刻 message_response。
func MessageResponse(model, text string, inputTokens, outputTokens int, tools any) map[string]any {
	content, stopReason := ContentBlocks(text, tools)
	return map[string]any{
		"id":            "msg_" + newUUIDValue(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}
}

// StreamAnthropicEvents 复刻 stream_events：把 OpenAI chunk 流转成 Anthropic 事件流。
//
// 状态机要点：
//   - 非工具模式：先开一个 text block，逐段追加 text_delta
//   - 工具模式：只在「确定不是工具调用」时才流文本，一旦出现工具标记就切到缓冲模式
//   - 结束时统一发 content_block_stop / message_delta / message_stop
func StreamAnthropicEvents(
	chunks []map[string]any,
	model string,
	inputTokens int,
	countOutputTokens func(string) int,
	tools any,
) []map[string]any {
	events := make([]map[string]any, 0, len(chunks)+8)
	messageID := "msg_" + newUUIDValue()
	created := NowUnix()

	toolMode := false
	if list, ok := tools.([]any); ok && len(list) > 0 {
		toolMode = true
	}

	events = append(events, map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": messageID, "type": "message", "role": "assistant",
			"model": model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
		},
	})

	textOpen := false
	if !toolMode {
		textOpen = true
		events = append(events, map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}

	currentText := ""
	streamedText := ""
	toolStarted := false

	for _, chunk := range chunks {
		delta, finishReason := chunkDeltaAndFinish(chunk)
		textDelta := stringOf(delta["content"])

		if textDelta != "" {
			currentText += textDelta
			if !toolStarted {
				visible := currentText
				if toolMode {
					visible = StreamableText(currentText)
				}
				if strings.HasPrefix(visible, streamedText) {
					emit := visible[len(streamedText):]
					if emit != "" {
						if !textOpen {
							textOpen = true
							events = append(events, map[string]any{
								"type": "content_block_start", "index": 0,
								"content_block": map[string]any{"type": "text", "text": ""},
							})
						}
						streamedText = visible
						events = append(events, map[string]any{
							"type": "content_block_delta", "index": 0,
							"delta": map[string]any{"type": "text_delta", "text": emit},
						})
					}
				}
				toolStarted = toolMode && visible != currentText
			}
		}

		if finishReason != nil {
			content, stopReason := ContentBlocks(currentText, tools)
			if textOpen {
				events = append(events, map[string]any{"type": "content_block_stop", "index": 0})
			}
			if stopReason == "tool_use" {
				startIndex := 0
				if textOpen {
					startIndex = 1
				}
				if len(content) > 0 && stringOf(content[0]["type"]) == "text" {
					remaining := stringOf(content[0]["text"])
					if len(remaining) >= len(streamedText) {
						remaining = remaining[len(streamedText):]
					}
					if remaining != "" {
						if !textOpen {
							events = append(events, map[string]any{
								"type": "content_block_start", "index": 0,
								"content_block": map[string]any{"type": "text", "text": ""},
							})
						}
						events = append(events, map[string]any{
							"type": "content_block_delta", "index": 0,
							"delta": map[string]any{"type": "text_delta", "text": remaining},
						})
						if !textOpen {
							events = append(events, map[string]any{"type": "content_block_stop", "index": 0})
						}
					}
					startIndex = 1
					content = content[1:]
				}
				events = append(events, streamBufferedBlocks(content, startIndex)...)
			}
			events = append(events, map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
				"usage": map[string]any{"output_tokens": countOutputTokens(currentText)},
			})
			break
		}
	}

	events = append(events, map[string]any{"type": "message_stop", "created": created})
	return events
}

// streamBufferedBlocks 复刻 _stream_buffered_blocks。
func streamBufferedBlocks(content []map[string]any, startIndex int) []map[string]any {
	events := make([]map[string]any, 0, len(content)*3)
	for offset, block := range content {
		index := startIndex + offset
		var start, delta map[string]any
		if stringOf(block["type"]) == "tool_use" {
			start = map[string]any{
				"type": "tool_use", "id": block["id"],
				"name": block["name"], "input": map[string]any{},
			}
			input := block["input"]
			if input == nil {
				input = map[string]any{}
			}
			encoded, _ := json.Marshal(input)
			delta = map[string]any{"type": "input_json_delta", "partial_json": string(encoded)}
		} else {
			start = map[string]any{"type": "text", "text": ""}
			delta = map[string]any{"type": "text_delta", "text": stringOf(block["text"])}
		}
		events = append(events,
			map[string]any{"type": "content_block_start", "index": index, "content_block": start},
			map[string]any{"type": "content_block_delta", "index": index, "delta": delta},
			map[string]any{"type": "content_block_stop", "index": index},
		)
	}
	return events
}

// chunkDeltaAndFinish 从 chunk 里取出 (delta, finish_reason)。
func chunkDeltaAndFinish(chunk map[string]any) (map[string]any, any) {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return map[string]any{}, nil
	}
	first, _ := choices[0].(map[string]any)
	if first == nil {
		return map[string]any{}, nil
	}
	delta, _ := first["delta"].(map[string]any)
	if delta == nil {
		delta = map[string]any{}
	}
	return delta, first["finish_reason"]
}
