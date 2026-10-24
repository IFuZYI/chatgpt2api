package protocol

import (
	"encoding/base64"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// 本文件实现 /v1/chat/completions 的「图片形态」：从 chat 请求里抽出 prompt 与
// 参考图，把生图结果转成 chat completion 响应。
//
// Python 权威实现：
//   - utils/helper.py:302-320  extract_prompt_from_message_content
//   - utils/helper.py:372-428  _decode_message_image_object / extract_image_from_message_content / extract_chat_image
//   - utils/helper.py:431-447  extract_chat_prompt
//   - utils/helper.py:450-460  parse_image_count
//   - utils/helper.py:460-469  build_chat_image_markdown_content
//   - services/protocol/openai_v1_chat_complete.py:164-287
//     chat_image_args / image_result_content / stream_image_chat_completion
//
// 两处必须保住的非对称语义：
//   - extract_chat_prompt 遍历**全部** user 消息并用 "\n" 拼接（不是只取最后一条）
//   - extract_chat_image 逆序遍历 messages，取**第一条**含图 user 消息的那一批图
//     （不是把所有消息里的图都收进来）

// ChatImageError 对应 Python 在 chat 图片链路里抛的
// HTTPException(400, {"error": ...})：prompt 缺失、n 越界、图片解码失败。
type ChatImageError struct {
	Message string
}

func (e *ChatImageError) Error() string { return e.Message }

// chatImageError 构造一条 chat 图片链路的 400 错误。
func chatImageError(message string) error {
	return &ChatImageError{Message: message}
}

// SUPPORTED_JSON_IMAGE_MIME_TYPES（utils/helper.py:28）与单图上限（:29）。
var supportedChatImageMIMETypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/jpg":  true,
	"image/webp": true,
	"image/gif":  true,
}

const maxChatImageBytes = 10 * 1024 * 1024

// DATA_URL_IMAGE_RE（utils/helper.py:31）：必须带 ";base64,"，否则走裸 base64 分支。
var chatDataURLImageRe = regexp.MustCompile(`(?s)^data:([-+./\w]+);base64,(.*)$`)

// chatImagePayload 是一张解出来的参考图（Python 的 (bytes, mime) 二元组）。
type chatImagePayload struct {
	data []byte
	mime string
}

// ExtractChatPrompt 复刻 extract_chat_prompt（utils/helper.py:431-447）。
//
// 优先级：body.prompt 非空直接用；否则遍历**全部** role=user 的消息，
// 逐条抽文本，用 "\n" 拼接后再 strip。
func ExtractChatPrompt(body map[string]any) string {
	if direct := strings.TrimSpace(stringOf(body["prompt"])); direct != "" {
		return direct
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(messages))
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(stringOf(message["role"]))) != "user" {
			continue
		}
		if prompt := extractPromptFromMessageContent(message["content"]); prompt != "" {
			parts = append(parts, prompt)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// extractPromptFromMessageContent 复刻 extract_prompt_from_message_content
// （utils/helper.py:302-320）。
//
// 字符串内容直接 strip；数组内容只认 type=text / input_text 两种 part。
func extractPromptFromMessageContent(content any) string {
	if text, ok := content.(string); ok {
		return strings.TrimSpace(text)
	}
	parts, ok := content.([]any)
	if !ok {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		item, ok := part.(map[string]any)
		if !ok {
			continue
		}
		switch strings.TrimSpace(stringOf(item["type"])) {
		case "text":
			if text := strings.TrimSpace(stringOf(item["text"])); text != "" {
				texts = append(texts, text)
			}
		case "input_text":
			value := chatValueOr(item["text"], item["input_text"])
			if text := strings.TrimSpace(stringOf(value)); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}

// ExtractChatImage 复刻 extract_chat_image（utils/helper.py:416-428）。
//
// 逆序遍历 messages，返回**第一条**含图 user 消息的那一批图；其余消息里的图
// 全部丢弃（Python 是 `for message in reversed(messages)` + `if images: return images`）。
//
// 返回三元组 (base64, filename, mime)：filename 按 Python chat_image_args 的
// 约定固定为 image_{idx}.png（与 mime 无关）。
//
// 与 Python 的差异只有一处：Python 解码失败抛 HTTPException，这里返回 error
// 让调用方映射成同样的 400（Go 的 protocol 层不构造 HTTP 响应）。
func ExtractChatImage(body map[string]any) ([][3]string, error) {
	messages, ok := body["messages"].([]any)
	if !ok {
		return nil, nil
	}
	for index := len(messages) - 1; index >= 0; index-- {
		message, ok := messages[index].(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(stringOf(message["role"]))) != "user" {
			continue
		}
		images, err := extractImagesFromMessageContent(message["content"])
		if err != nil {
			return nil, err
		}
		if len(images) > 0 {
			return chatImageTuples(images), nil
		}
	}
	return nil, nil
}

// chatImageTuples 把解码结果转成 ImageRequest.Images 的三元组。
//
// 空 data 的条目要丢掉：Python 在 chat_image_args 之后走
// `encode_images(images) or None`，encode_images 里的 `if data` 会过滤空图。
func chatImageTuples(images []chatImagePayload) [][3]string {
	out := make([][3]string, 0, len(images))
	for index, image := range images {
		if len(image.data) == 0 {
			continue
		}
		out = append(out, [3]string{
			base64.StdEncoding.EncodeToString(image.data),
			fmt.Sprintf("image_%d.png", index+1),
			image.mime,
		})
	}
	return out
}

// extractImagesFromMessageContent 复刻 extract_image_from_message_content
// （utils/helper.py:397-413）。
func extractImagesFromMessageContent(content any) ([]chatImagePayload, error) {
	parts, ok := content.([]any)
	if !ok {
		return nil, nil
	}
	images := make([]chatImagePayload, 0, len(parts))
	for _, part := range parts {
		item, ok := part.(map[string]any)
		if !ok {
			continue
		}
		switch strings.TrimSpace(stringOf(item["type"])) {
		case "image_url":
			image, found, err := decodeMessageImageURL(chatValueOr(item["image_url"], item["url"], item))
			if err != nil {
				return nil, err
			}
			if found {
				images = append(images, image)
			}
		case "input_image", "image":
			image, found, err := decodeMessageImageObject(item)
			if err != nil {
				return nil, err
			}
			if found {
				images = append(images, image)
			}
		}
	}
	return images, nil
}

// decodeMessageImageURL 复刻 _decode_message_image_url（utils/helper.py:318-370）。
//
// data URL 直接解（mime 取 header，宽松 base64：Python 这里用 validate=False）；
// 裸字符串不是 URL 就跳过（found=false）；远程 URL 在 Go 侧未接入，按现有
// 生图接口的口径报 400。
func decodeMessageImageURL(value any) (chatImagePayload, bool, error) {
	source := chatMessageImageURL(value)
	if source == "" {
		return chatImagePayload{}, false, nil
	}
	if strings.HasPrefix(source, "data:") {
		header, encoded, _ := strings.Cut(source, ",")
		mime := strings.TrimPrefix(strings.SplitN(header, ";", 2)[0], "data:")
		if mime == "" {
			mime = "image/png"
		}
		payload, err := decodeChatBase64(encoded)
		if err != nil {
			return chatImagePayload{}, false, chatImageError("invalid base64 image data")
		}
		return chatImagePayload{data: payload, mime: mime}, true, nil
	}
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		return chatImagePayload{}, false, chatImageError("remote image URLs are not supported yet")
	}
	return chatImagePayload{}, false, nil
}

// chatMessageImageURL 复刻 _message_image_url（utils/helper.py:308-312）：
// 对象取 url / image_url，其余按字符串处理。
func chatMessageImageURL(value any) string {
	if object, ok := value.(map[string]any); ok {
		if url := stringOf(object["url"]); url != "" {
			return strings.TrimSpace(url)
		}
		return strings.TrimSpace(stringOf(object["image_url"]))
	}
	return strings.TrimSpace(stringOf(value))
}

// decodeMessageImageObject 复刻 _decode_message_image_object（utils/helper.py:372-393）。
//
// 依次尝试 image_url/url → b64_json/base64 → source.type=base64。
// 注意 image_url 与 url 是**逐键尝试**：image_url 存在但解不出来时还要看 url
// （Python 的 `for key in ("image_url", "url")` + `if image: return image`）。
func decodeMessageImageObject(item map[string]any) (chatImagePayload, bool, error) {
	for _, key := range []string{"image_url", "url"} {
		image, found, err := decodeMessageImageURL(item[key])
		if err != nil || found {
			return image, found, err
		}
	}

	value := chatValueOr(item["b64_json"], item["base64"])
	if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
		mime := stringOf(chatValueOr(item["mime"], item["mime_type"], item["mimeType"]))
		image, err := decodeJSONChatImageString(text, mime)
		if err != nil {
			return chatImagePayload{}, false, err
		}
		return image, true, nil
	}

	if source, ok := item["source"].(map[string]any); ok && stringOf(source["type"]) == "base64" {
		mime := stringOf(chatValueOr(source["media_type"], source["mime_type"]))
		image, err := decodeJSONChatImageString(stringOf(source["data"]), mime)
		if err != nil {
			return chatImagePayload{}, false, err
		}
		return image, true, nil
	}
	return chatImagePayload{}, false, nil
}

// decodeJSONChatImageString 复刻 _decode_json_image_string（utils/helper.py:40-65）。
//
// 与 decodeMessageImageURL 的 data URL 分支不同：这里校验 mime 白名单、用严格
// base64（Python validate=True）、并限制 10MB。
func decodeJSONChatImageString(value, mimeType string) (chatImagePayload, error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return chatImagePayload{}, chatImageError("image file is empty")
	}

	resolvedMIME := ""
	encoded := ""
	if match := chatDataURLImageRe.FindStringSubmatch(text); match != nil {
		resolvedMIME = strings.ToLower(orDefaultString(match[1], "image/png"))
		encoded = match[2]
	} else {
		if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
			return chatImagePayload{}, chatImageError("remote image URLs are not supported")
		}
		resolvedMIME = strings.ToLower(orDefaultString(mimeType, "image/png"))
		encoded = text
	}

	if resolvedMIME == "image/jpg" {
		resolvedMIME = "image/jpeg"
	}
	if !supportedChatImageMIMETypes[resolvedMIME] {
		return chatImagePayload{}, chatImageError("unsupported image mime type")
	}

	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return chatImagePayload{}, chatImageError("invalid base64 image data")
	}
	if len(payload) == 0 {
		return chatImagePayload{}, chatImageError("image file is empty")
	}
	if len(payload) > maxChatImageBytes {
		return chatImagePayload{}, chatImageError("image file is too large")
	}
	return chatImagePayload{data: payload, mime: resolvedMIME}, nil
}

// decodeChatBase64 复刻 base64.b64decode(validate=False)：丢弃非字母表字符，
// 但 padding 仍必须正确（Python 在丢弃字符后照样做 padding 检查）。
func decodeChatBase64(value string) ([]byte, error) {
	filtered := make([]byte, 0, len(value))
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'A' && character <= 'Z',
			character >= 'a' && character <= 'z',
			character >= '0' && character <= '9',
			character == '+', character == '/', character == '=':
			filtered = append(filtered, character)
		}
	}
	return base64.StdEncoding.DecodeString(string(filtered))
}

// ParseImageCount 复刻 parse_image_count（utils/helper.py:450-460）。
//
// 注意与 /v1/images/generations 的 pydantic 校验**不同**：Python 这里是
// `int(raw_value or 1)`，所以数值 0 / "" / false / null 都会被当成 1，
// 而字符串 "0" 会走 int("0") = 0 并报越界。Go 侧按同一语义实现。
func ParseImageCount(raw any) (int, error) {
	value := 1
	if chatTruthy(raw) {
		parsed, ok := chatIntValue(raw)
		if !ok {
			return 0, chatImageError("n must be an integer")
		}
		value = parsed
	}
	if value < 1 || value > 4 {
		return 0, chatImageError("n must be between 1 and 4")
	}
	return value, nil
}

// chatTruthy 复刻 Python 的真值判断（只覆盖 JSON 能出现的类型）。
func chatTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0
	case int:
		return typed != 0
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

// chatIntValue 复刻 Python 的 int(value)：浮点截断、布尔当 0/1、字符串去空白后解析。
func chatIntValue(value any) (int, bool) {
	switch typed := value.(type) {
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		return int(typed), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// ChatImageArgs 复刻 chat_image_args（openai_v1_chat_complete.py:164-173）。
//
// 校验顺序与 Python 一致：prompt 缺失 → 参考图解码失败 → n 越界。
func ChatImageArgs(body map[string]any) (model, prompt string, n int, images [][3]string, err error) {
	model = strings.TrimSpace(stringOf(body["model"]))
	if model == "" {
		model = "gpt-image-2"
	}

	prompt = ExtractChatPrompt(body)
	if prompt == "" {
		return "", "", 0, nil, chatImageError("prompt is required")
	}

	images, err = ExtractChatImage(body)
	if err != nil {
		return "", "", 0, nil, err
	}

	n, err = ParseImageCount(body["n"])
	if err != nil {
		return "", "", 0, nil, err
	}
	return model, prompt, n, images, nil
}

// BuildChatImageMarkdownContent 复刻 build_chat_image_markdown_content
// （utils/helper.py:460-469）。
//
// 图片序号按 data 数组下标（含被跳过的非对象 / 无 b64 条目），多图用 "\n\n" 拼接。
func BuildChatImageMarkdownContent(data []any) string {
	markdownImages := make([]string, 0, len(data))
	for index, item := range data {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		encoded := strings.TrimSpace(stringOf(entry["b64_json"]))
		if encoded == "" {
			continue
		}
		markdownImages = append(markdownImages, fmt.Sprintf(
			"![image_%d](data:image/png;base64,%s)", index+1, encoded))
	}
	if len(markdownImages) == 0 {
		return "Image generation completed."
	}
	return strings.Join(markdownImages, "\n\n")
}

// ImageResultContent 复刻 image_result_content（openai_v1_chat_complete.py:225-229）。
func ImageResultContent(result map[string]any) string {
	if data, ok := result["data"].([]any); ok && len(data) > 0 {
		return BuildChatImageMarkdownContent(data)
	}
	if message := stringOf(result["message"]); message != "" {
		return message
	}
	return "Image generation completed."
}

// ImageChatCompletion 复刻 image_chat_response 的响应组装部分
// （openai_v1_chat_complete.py:232-248）：把 collect_image_outputs 的结果
// 转成 chat.completion，usage 换成图片口径。
func ImageChatCompletion(model string, result map[string]any, prompt string, images [][3]string) map[string]any {
	created := int64(intValueOf(result["created"]))
	if created == 0 {
		created = NowUnix()
	}
	response := ChatCompletionResponse(model, ImageResultContent(result), created, 0, 0, 0, nil)
	response["usage"] = ChatUsageFromImageUsage(ImageUsage(
		CountTextTokens(prompt, model),
		CountImageInputsTokens(images, model, "auto"),
		CountImageOutputItemsTokens(imageResultItems(result["data"]), nil, "auto"),
	))
	return response
}

// imageResultItems 把 collected["data"] 收窄成 usage 计算需要的对象列表。
func imageResultItems(data any) []map[string]any {
	items := make([]map[string]any, 0)
	list, ok := data.([]any)
	if !ok {
		return items
	}
	for _, item := range list {
		if entry, ok := item.(map[string]any); ok {
			items = append(items, entry)
		}
	}
	return items
}

// ChatImageChunkStream 复刻 stream_image_chat_completion
// （openai_v1_chat_complete.py:263-287）的状态机。
//
// 做成有状态类型而不是一次性的切片函数，是为了让 API 层能按 output 到达顺序
// 逐条转换（调用方决定缓冲还是边产边写）。语义逐条对齐 Python：
//   - 首块带 role，之后只带 content
//   - progress 文本累积到 sentText
//   - message 若以 sentText 开头就只发增量（Python: output.text[len(sent_text):]）
//   - 一条内容都没有时补一个空 role 块，最后一定发 finish_reason=stop
type ChatImageChunkStream struct {
	model        string
	completionID string
	created      int64
	sentRole     bool
	sentText     string
}

// NewChatImageChunkStream 创建一个流式转换器（completion id 与 created 在
// 构造时固定，保证同一次响应里所有 chunk 的 id/created 一致）。
func NewChatImageChunkStream(model string) *ChatImageChunkStream {
	return &ChatImageChunkStream{
		model:        model,
		completionID: CompletionID(),
		created:      NowUnix(),
	}
}

// Chunk 把一条 output 转成 chunk。ok=false 表示这条 output 不产生内容
// （Python 的 `if not content: continue`）。
func (s *ChatImageChunkStream) Chunk(output ImageOutput) (map[string]any, bool) {
	content := ""
	switch output.Kind {
	case ImageKindProgress:
		content = output.Text
		s.sentText += content
	case ImageKindResult:
		content = BuildChatImageMarkdownContent(imageOutputData(output.Data))
	case ImageKindMessage:
		if strings.HasPrefix(output.Text, s.sentText) {
			content = output.Text[len(s.sentText):]
		} else {
			content = output.Text
		}
	}
	if content == "" {
		return nil, false
	}
	return s.emit(content), true
}

// Finish 产出收尾 chunk 序列。
func (s *ChatImageChunkStream) Finish() []map[string]any {
	chunks := make([]map[string]any, 0, 2)
	if !s.sentRole {
		chunks = append(chunks, ChatCompletionChunk(s.model,
			map[string]any{"role": "assistant", "content": ""}, nil, s.completionID, s.created))
		s.sentRole = true
	}
	chunks = append(chunks, ChatCompletionChunk(s.model,
		map[string]any{}, "stop", s.completionID, s.created))
	return chunks
}

// emit 按「首块带 role」的规则产出 chunk。
func (s *ChatImageChunkStream) emit(content string) map[string]any {
	delta := map[string]any{"content": content}
	if !s.sentRole {
		s.sentRole = true
		delta = map[string]any{"role": "assistant", "content": content}
	}
	return ChatCompletionChunk(s.model, delta, nil, s.completionID, s.created)
}

// imageOutputData 把 ImageOutput.Data 转成 BuildChatImageMarkdownContent 的入参。
func imageOutputData(data []map[string]any) []any {
	items := make([]any, 0, len(data))
	for _, item := range data {
		items = append(items, item)
	}
	return items
}

// chatValueOr 复刻 Python 的 `a or b or c`：跳过 nil、空串、空对象。
func chatValueOr(values ...any) any {
	for _, value := range values {
		switch typed := value.(type) {
		case nil:
			continue
		case string:
			if typed == "" {
				continue
			}
		case map[string]any:
			if len(typed) == 0 {
				continue
			}
		case []any:
			if len(typed) == 0 {
				continue
			}
		}
		return value
	}
	return nil
}
