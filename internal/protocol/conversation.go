package protocol

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ConversationPayload 复刻 _conversation_payload 的请求体。
//
// 字段集是上游契约的一部分：少一个字段上游可能仍然接受，但
// 行为会不同（例如缺 conversation_mode 会走错模型路由）。
func ConversationPayload(messages []map[string]any, model, timezone, thinkingEffort string) map[string]any {
	payload := map[string]any{
		"action":                        "next",
		"messages":                      messages,
		"model":                         model,
		"parent_message_id":             newUUIDValue(),
		"conversation_mode":             map[string]any{"kind": "primary_assistant"},
		"conversation_origin":           nil,
		"force_paragen":                 false,
		"force_paragen_model_slug":      "",
		"force_rate_limit":              false,
		"force_use_sse":                 true,
		"history_and_training_disabled": true,
		"reset_rate_limits":             false,
		"suggestions":                   []any{},
		"supported_encodings":           []any{},
		"system_hints":                  []any{},
		"timezone":                      timezone,
		"timezone_offset_min":           -480,
		"variant_purpose":               "comparison_implicit",
		"websocket_request_id":          newUUIDValue(),
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": 120,
			"page_height":       900,
			"page_width":        1400,
			"pixel_ratio":       2,
			"screen_height":     1440,
			"screen_width":      2560,
		},
	}
	if effort := NormalizeThinkingEffort(thinkingEffort); effort != "" {
		payload["thinking_effort"] = effort
	}
	return payload
}

// APIMessagesToConversation 复刻 _api_messages_to_conversation_messages。
//
// 纯文本消息走 content_type=text；带图片的消息需要先把图片上传到
// 上游拿到 file_id（由调用方通过 uploader 注入），再拼 multimodal_text。
func APIMessagesToConversation(
	messages []map[string]any,
	uploader func(payload []byte, filename, mime string) (map[string]any, error),
	authenticated bool,
) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(messages))
	for _, item := range messages {
		role := "user"
		if value, ok := item["role"].(string); ok && value != "" {
			role = value
		}
		content := item["content"]

		text, ok := content.(string)
		if ok {
			out = append(out, map[string]any{
				"id":      newUUIDValue(),
				"author":  map[string]any{"role": role},
				"content": map[string]any{"content_type": "text", "parts": []any{text}},
			})
			continue
		}

		parts, ok := content.([]any)
		if !ok {
			return nil, fmt.Errorf("only string or list message content is supported")
		}

		textParts := make([]string, 0, len(parts))
		type imageInput struct {
			data []byte
			mime string
		}
		images := make([]imageInput, 0, len(parts))
		for _, part := range parts {
			entry, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch stringOf(entry["type"]) {
			case "text", "input_text", "output_text":
				// Responses API 的内容块类型是 input_text / output_text，
				// chat 用的是 text。只认 text 会让 /v1/responses 的文本
				// 整段丢失 —— 上游收到 parts:[""]，用户问题根本没发出去。
				textParts = append(textParts, stringOf(entry["text"]))
			case "image":
				mime := stringOf(entry["mime"])
				if mime == "" {
					mime = "image/png"
				}
				if data, ok := entry["data"].([]byte); ok {
					images = append(images, imageInput{data: data, mime: mime})
				}
			}
		}

		if len(images) == 0 {
			out = append(out, map[string]any{
				"id":      newUUIDValue(),
				"author":  map[string]any{"role": role},
				"content": map[string]any{"content_type": "text", "parts": []any{strings.Join(textParts, "")}},
			})
			continue
		}

		if !authenticated {
			return nil, fmt.Errorf("authenticated upstream account required for image input")
		}
		if uploader == nil {
			return nil, fmt.Errorf("image uploader is not configured")
		}

		uploaded := make([]map[string]any, 0, len(images))
		for index, image := range images {
			extension := mimeExtension(image.mime)
			ref, err := uploader(image.data, fmt.Sprintf("image_%d.%s", index+1, extension), image.mime)
			if err != nil {
				return nil, err
			}
			uploaded = append(uploaded, ref)
		}

		partsOut := make([]any, 0, len(uploaded)+1)
		for _, ref := range uploaded {
			partsOut = append(partsOut, map[string]any{
				"content_type":  "image_asset_pointer",
				"asset_pointer": "file-service://" + stringOf(ref["file_id"]),
				"width":         ref["width"],
				"height":        ref["height"],
				"size_bytes":    ref["file_size"],
			})
		}
		if text := strings.Join(textParts, ""); text != "" {
			partsOut = append(partsOut, text)
		}

		attachments := make([]any, 0, len(uploaded))
		for _, ref := range uploaded {
			attachments = append(attachments, map[string]any{
				"id":       ref["file_id"],
				"mimeType": ref["mime_type"],
				"name":     ref["file_name"],
				"size":     ref["file_size"],
				"width":    ref["width"],
				"height":   ref["height"],
			})
		}

		out = append(out, map[string]any{
			"id":      newUUIDValue(),
			"author":  map[string]any{"role": role},
			"content": map[string]any{"content_type": "multimodal_text", "parts": partsOut},
			"metadata": map[string]any{
				"attachments": attachments,
			},
		})
	}
	return out, nil
}

func mimeExtension(mime string) string {
	_, subtype, found := strings.Cut(mime, "/")
	if !found {
		return "png"
	}
	if index := strings.IndexByte(subtype, '+'); index >= 0 {
		subtype = subtype[:index]
	}
	if subtype == "jpeg" {
		return "jpg"
	}
	if subtype == "" {
		return "png"
	}
	return subtype
}

// NormalizeThinkingEffort 复刻 _normalize_thinking_effort。
func NormalizeThinkingEffort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none", "auto":
		return ""
	case "low", "medium", "high", "standard", "max":
		return strings.ToLower(strings.TrimSpace(value))
	case "xhigh", "extended":
		return "extended"
	default:
		return ""
	}
}

// ChatTarget 复刻 _chat_target：登录态与匿名态走不同的路径与时区。
func ChatTarget(authenticated bool) (path, timezone string) {
	if authenticated {
		return "/backend-api/conversation", "Asia/Shanghai"
	}
	return "/backend-anon/conversation", "America/Los_Angeles"
}

// Upstream 是 protocol 层对上游客户端的最小依赖面。
//
// 刻意用接口而不是直接 import upstream：upstream 需要 protocol 的
// 类型（ConversationPayload 之类），双向依赖会成环。
type Upstream interface {
	// ChatRequirements 完成 sentinel 握手。
	ChatRequirements(ctx context.Context) (Requirements, error)
	// DoStream 发起一个 SSE 请求，返回响应体（调用方负责关闭）。
	DoStream(ctx context.Context, path string, payload []byte, requirements Requirements) (io.ReadCloser, error)
	// Authenticated 表示当前是否登录态（决定 conversation 路径与时区）。
	Authenticated() bool
	// UploadImage 把图片上传到上游，返回 file_id/尺寸等。
	UploadImage(payload []byte, filename, mime string) (map[string]any, error)
}

// Requirements 是 sentinel 握手结果。
type Requirements struct {
	Token          string
	ProofToken     string
	TurnstileToken string
	SOToken        string
	Raw            map[string]any
}

// StreamConversationWithHints 与 StreamConversation 相同，但支持 system_hints
// 与图片引用。图片链路必须传 system_hints=["picture_v2"]，否则上游不走画图。
func StreamConversationWithHints(
	ctx context.Context,
	client Upstream,
	messages []map[string]any,
	model string,
	thinkingEffort string,
	images [][3]string,
	systemHints []string,
) (<-chan string, <-chan error, error) {
	requirements, err := client.ChatRequirements(ctx)
	if err != nil {
		return nil, nil, err
	}

	authenticated := client.Authenticated()
	path, timezone := ChatTarget(authenticated)

	conversationMessages := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		conversationMessages = append(conversationMessages, copyMapAny(message))
	}

	payload := ConversationPayload(conversationMessages, model, timezone, thinkingEffort)
	if len(systemHints) > 0 {
		hints := make([]any, 0, len(systemHints))
		for _, hint := range systemHints {
			hints = append(hints, hint)
		}
		payload["system_hints"] = hints
	}

	// 图片引用要转成 file-service:// 指针（先上传）。
	if len(images) > 0 {
		if !authenticated {
			return nil, nil, fmt.Errorf("access_token is required for image endpoints")
		}
		parts := make([]any, 0, len(images)+1)
		for _, image := range images {
			payloadBytes, decodeErr := base64.StdEncoding.DecodeString(image[0])
			if decodeErr != nil {
				continue
			}
			ref, uploadErr := client.UploadImage(payloadBytes, image[1], image[2])
			if uploadErr != nil {
				return nil, nil, uploadErr
			}
			parts = append(parts, map[string]any{
				"content_type":  "image_asset_pointer",
				"asset_pointer": "file-service://" + stringOf(ref["file_id"]),
				"width":         ref["width"],
				"height":        ref["height"],
				"size_bytes":    ref["file_size"],
			})
		}
		if len(parts) > 0 {
			payload["messages"] = []any{map[string]any{
				"id":      newUUIDValue(),
				"author":  map[string]any{"role": "user"},
				"content": map[string]any{"content_type": "multimodal_text", "parts": parts},
			}}
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}

	resp, err := client.DoStream(ctx, path, body, requirements)
	if err != nil {
		return nil, nil, err
	}

	payloads := make(chan string, 64)
	errs := make(chan error, 1)
	go func() {
		defer close(payloads)
		defer close(errs)
		defer resp.Close()
		if readErr := readSSE(resp, payloads); readErr != nil {
			errs <- readErr
		}
	}()
	return payloads, errs, nil
}

// StreamConversation 发起一次对话并返回 SSE payload 迭代器。
//
// 返回的 channel 在流结束或出错时关闭；error 通过第二个返回值给出。
// 调用方必须消费完 channel 或取消 ctx，否则 goroutine 泄漏。
func StreamConversation(
	ctx context.Context,
	client Upstream,
	messages []map[string]any,
	model string,
	thinkingEffort string,
) (<-chan string, <-chan error, error) {
	requirements, err := client.ChatRequirements(ctx)
	if err != nil {
		return nil, nil, err
	}

	authenticated := client.Authenticated()
	path, timezone := ChatTarget(authenticated)
	conversationMessages, err := APIMessagesToConversation(messages, client.UploadImage, authenticated)
	if err != nil {
		return nil, nil, err
	}
	payload := ConversationPayload(conversationMessages, model, timezone, thinkingEffort)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}

	resp, err := client.DoStream(ctx, path, body, requirements)
	if err != nil {
		return nil, nil, err
	}

	payloads := make(chan string, 64)
	errs := make(chan error, 1)
	go func() {
		defer close(payloads)
		defer close(errs)
		defer resp.Close()
		if err := readSSE(resp, payloads); err != nil {
			errs <- err
		}
	}()
	return payloads, errs, nil
}

// readSSE 逐行读 SSE，只把 data: 之后的内容送出去。
func readSSE(body io.Reader, out chan<- string) error {
	reader := bufio.NewReaderSize(body, 64*1024)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(trimmed[len("data:"):])
				if payload != "" {
					select {
					case out <- payload:
					case <-time.After(0):
						// 非阻塞发送会丢数据，所以这里其实一直阻塞；
						// 保留 select 形态便于将来接入 ctx。
						out <- payload
					}
					if payload == "[DONE]" {
						return nil
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
