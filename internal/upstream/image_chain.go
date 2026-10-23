package upstream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"

	"github.com/basketikun/chatgpt2api/internal/protocol"
)

// 图片链路走的是 /backend-api/f/conversation 系列端点，不是 /backend-api/conversation。
//
// 差异（实测 + 读 Python 源码确认）：
//   - 图片要先用 /f/conversation/prepare 换一个 conduit_token
//   - 生成请求必须带 system_hints=["picture_v2"] 与 client_prepare_state
//   - 响应是 SSE，但事件流里图片结果以 file_id/sediment_id 形式出现
//
// 走错端点会稳定拿到上游 5xx（实测 0.6s 就失败）。

// imageModelSettings 复刻 _image_model_settings：
// 把标准图片模型名映射成上游模型名与思考强度。
func (c *Client) imageModelSettings(model string, defaultUpstream string, defaultEffort string) (string, string) {
	_, baseModel := protocol.SplitImageModel(model)
	if baseModel == "" {
		return "auto", ""
	}
	var upstreamModel string
	switch baseModel {
	case "gpt-image-2":
		// gpt-image-2 是「通用出图」入口：上游模型由配置决定（默认 gpt-5-5），
		// 这样用户可以用 default_upstream_model_name 切换底层模型。
		if defaultUpstream == "" {
			defaultUpstream = "gpt-5-5"
		}
		upstreamModel = defaultUpstream
	case protocol.CodexImageModel:
		upstreamModel = baseModel
	default:
		// 2.5 系列（gpt-image-2.5 / -flare / -sunburst）：模型名本身就是上游 slug，
		// 直接透传。实测把这三个名字作为 upstream_model 传给 /f/conversation
		// 能正常出图（三者字节数各异，确认是不同模型）。
		// 不要走 defaultUpstream 映射 —— 那会把它们全变成 gpt-5-5，退化成同一个模型。
		if protocol.IsSupportedImageModel(baseModel) {
			upstreamModel = baseModel
		} else {
			return "auto", ""
		}
	}

	// 支持 "gpt-5-5-standard" 这类后缀覆盖思考强度。
	name, suffix, found := cutLast(upstreamModel, "-")
	if found {
		switch suffix {
		case "standard", "extended", "max":
			return name, suffix
		}
	}
	return upstreamModel, protocol.NormalizeThinkingEffort(defaultEffort)
}

func cutLast(value, sep string) (string, string, bool) {
	index := -1
	for i := len(value) - len(sep); i >= 0; i-- {
		if value[i:i+len(sep)] == sep {
			index = i
			break
		}
	}
	if index < 0 {
		return value, "", false
	}
	return value[:index], value[index+len(sep):], true
}

// imageHeaders 复刻 _image_headers。
func (c *Client) imageHeaders(path string, req protocol.Requirements, conduitToken, accept string) http.Header {
	extra := map[string]string{
		"Content-Type": "application/json",
		"Accept":       accept,
		"OpenAI-Sentinel-Chat-Requirements-Token": req.Token,
	}
	if req.ProofToken != "" {
		extra["OpenAI-Sentinel-Proof-Token"] = req.ProofToken
	}
	if conduitToken != "" {
		extra["X-Conduit-Token"] = conduitToken
	}
	if accept == "text/event-stream" {
		extra["X-Oai-Turn-Trace-Id"] = newUUID()
	}
	return c.headers(path, extra)
}

// prepareImageConversation 复刻 _prepare_image_conversation：换 conduit_token。
func (c *Client) prepareImageConversation(
	ctx context.Context,
	prompt, model, defaultUpstream, defaultEffort string,
	req protocol.Requirements,
) (string, error) {
	upstreamModel, thinkingEffort := c.imageModelSettings(model, defaultUpstream, defaultEffort)

	payload := map[string]any{
		"action":                 "next",
		"fork_from_shared_post":  false,
		"parent_message_id":      newUUID(),
		"model":                  upstreamModel,
		"client_prepare_state":   "success",
		"timezone_offset_min":    -480,
		"timezone":               "Asia/Shanghai",
		"conversation_mode":      map[string]any{"kind": "primary_assistant"},
		"system_hints":           []any{"picture_v2"},
		"supports_buffering":     true,
		"supported_encodings":    []any{"v1"},
		"client_contextual_info": map[string]any{"app_name": "chatgpt.com"},
		"partial_query": map[string]any{
			"id":      newUUID(),
			"author":  map[string]any{"role": "user"},
			"content": map[string]any{"content_type": "text", "parts": []any{prompt}},
		},
	}
	if thinkingEffort != "" {
		payload["thinking_effort"] = thinkingEffort
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	path := "/backend-api/f/conversation/prepare"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+path, newBytesReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header = c.imageHeaders(path, req, "", "*/*")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if resp.StatusCode >= 400 {
		return "", NewStatusError(resp.StatusCode, nil,
			fmt.Sprintf("%s failed: status=%d body=%s", path, resp.StatusCode, truncate(string(responseBody), 300)))
	}

	var decoded map[string]any
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return "", fmt.Errorf("%s: invalid json: %w", path, err)
	}
	return stringOfValue(decoded["conduit_token"]), nil
}

// startImageGeneration 复刻 _start_image_generation：发起生成并返回 SSE 流。
func (c *Client) startImageGeneration(
	ctx context.Context,
	prompt, model, defaultUpstream, defaultEffort string,
	req protocol.Requirements,
	conduitToken string,
	references []map[string]any,
) (io.ReadCloser, error) {
	upstreamModel, thinkingEffort := c.imageModelSettings(model, defaultUpstream, defaultEffort)

	parts := make([]any, 0, len(references)+1)
	for _, ref := range references {
		parts = append(parts, map[string]any{
			"content_type":  "image_asset_pointer",
			"asset_pointer": "file-service://" + stringOfValue(ref["file_id"]),
			"width":         ref["width"],
			"height":        ref["height"],
			"size_bytes":    ref["file_size"],
		})
	}
	parts = append(parts, prompt)

	content := map[string]any{"content_type": "text", "parts": []any{prompt}}
	if len(references) > 0 {
		content = map[string]any{"content_type": "multimodal_text", "parts": parts}
	}

	metadata := map[string]any{
		"developer_mode_connector_ids": []any{},
		"selected_github_repos":        []any{},
		"selected_all_github_repos":    false,
		"system_hints":                 []any{"picture_v2"},
		"serialization_metadata":       map[string]any{"custom_symbol_offsets": []any{}},
	}
	if len(references) > 0 {
		attachments := make([]any, 0, len(references))
		for _, ref := range references {
			attachments = append(attachments, map[string]any{
				"id":       ref["file_id"],
				"mimeType": ref["mime_type"],
				"name":     ref["file_name"],
				"size":     ref["file_size"],
				"width":    ref["width"],
				"height":   ref["height"],
			})
		}
		metadata["attachments"] = attachments
	}

	payload := map[string]any{
		"action": "next",
		"messages": []any{map[string]any{
			"id":          newUUID(),
			"author":      map[string]any{"role": "user"},
			"create_time": float64(time.Now().UnixNano()) / 1e9,
			"content":     content,
			"metadata":    metadata,
		}},
		"parent_message_id":        newUUID(),
		"model":                    upstreamModel,
		"client_prepare_state":     "sent",
		"timezone_offset_min":      -480,
		"timezone":                 "Asia/Shanghai",
		"conversation_mode":        map[string]any{"kind": "primary_assistant"},
		"enable_message_followups": true,
		"system_hints":             []any{"picture_v2"},
		"supports_buffering":       true,
		"supported_encodings":      []any{"v1"},
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": 1200,
			"page_height":       1072,
			"page_width":        1724,
			"pixel_ratio":       1.2,
			"screen_height":     1440,
			"screen_width":      2560,
			"app_name":          "chatgpt.com",
		},
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
	}
	if thinkingEffort != "" {
		payload["thinking_effort"] = thinkingEffort
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	path := "/backend-api/f/conversation"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+path, newBytesReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header = c.imageHeaders(path, req, conduitToken, "text/event-stream")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, NewStatusError(resp.StatusCode, nil,
			fmt.Sprintf("%s failed: status=%d body=%s", path, resp.StatusCode, truncate(string(preview), 300)))
	}
	return resp.Body, nil
}

// StreamImageConversation 实现 protocol.ImageStreamer：
// 走完整的图片链路（prepare → generate）并返回 SSE payload 流。
func (c *Client) StreamImageConversation(
	ctx context.Context,
	prompt, model string,
	images [][3]string,
	references []map[string]any,
	defaultUpstream, defaultEffort string,
	onProgress func(string),
) (<-chan string, <-chan error, error) {
	if onProgress != nil {
		onProgress("bootstrapping")
	}
	if err := c.Bootstrap(ctx); err != nil {
		return nil, nil, err
	}
	if onProgress != nil {
		onProgress("getting_token")
	}
	rawReq, err := c.chatRequirements(ctx)
	if err != nil {
		return nil, nil, err
	}
	req := protocol.Requirements{
		Token:          rawReq.Token,
		ProofToken:     rawReq.ProofToken,
		TurnstileToken: rawReq.TurnstileToken,
		SOToken:        rawReq.SOToken,
		Raw:            rawReq.Raw,
	}

	// 图片引用要先上传换 file_id。
	uploaded := references
	if len(images) > 0 {
		if onProgress != nil {
			onProgress("uploading")
		}
		uploaded = make([]map[string]any, 0, len(images))
		for index, image := range images {
			payload, decodeErr := decodeBase64Image(image[0])
			if decodeErr != nil {
				continue
			}
			ref, uploadErr := c.UploadImage(payload, image[1], image[2])
			if uploadErr != nil {
				return nil, nil, uploadErr
			}
			uploaded = append(uploaded, ref)
			_ = index
		}
	}

	if onProgress != nil {
		onProgress("preparing_conversation")
	}
	conduit, err := c.prepareImageConversation(ctx, prompt, model, defaultUpstream, defaultEffort, req)
	if err != nil {
		return nil, nil, err
	}

	if onProgress != nil {
		onProgress("starting_generation")
	}
	stream, err := c.startImageGeneration(ctx, prompt, model, defaultUpstream, defaultEffort, req, conduit, uploaded)
	if err != nil {
		return nil, nil, err
	}

	if onProgress != nil {
		onProgress("generating")
	}

	payloads := make(chan string, 64)
	errs := make(chan error, 1)
	go func() {
		defer close(payloads)
		defer close(errs)
		defer stream.Close()
		if readErr := readSSEPayloads(stream, payloads); readErr != nil {
			errs <- readErr
		}
	}()
	return payloads, errs, nil
}

// readSSEPayloads 逐行读 SSE，只把 data: 之后的内容送出去。
//
// 与 protocol.readSSE 等价，但 upstream 不能反向依赖 protocol 的内部实现，
// 所以这里保留一份最小实现。
func readSSEPayloads(body io.Reader, out chan<- string) error {
	reader := bufio.NewReaderSize(body, 64*1024)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(trimmed[len("data:"):])
				if payload != "" {
					out <- payload
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

// StreamImage 实现 protocol.ImageUpstream.StreamImage：
// 按模型分流到两条独立链路，并把 SSE payload 转成 channel。
//
//   - codex 系列（codex-gpt-image-2 及 plus-/team-/pro- 前缀别名）
//     → /backend-api/codex/responses（Responses 协议，图片内联 base64）
//   - 其余图片模型 → /backend-api/f/conversation（prepare → generate）
func (c *Client) StreamImage(
	ctx context.Context,
	request protocol.ImageStreamRequest,
) (<-chan string, <-chan error, error) {
	if protocol.IsCodexImageModel(request.Model) {
		return c.streamCodexImage(ctx, request.Prompt, request.Model, request.Images,
			request.Size, request.Quality, request.Progress)
	}

	if err := c.Bootstrap(ctx); err != nil {
		return nil, nil, err
	}
	if request.Progress != nil {
		request.Progress("getting_token")
	}
	rawReq, err := c.chatRequirements(ctx)
	if err != nil {
		return nil, nil, err
	}
	req := protocol.Requirements{
		Token:          rawReq.Token,
		ProofToken:     rawReq.ProofToken,
		TurnstileToken: rawReq.TurnstileToken,
		SOToken:        rawReq.SOToken,
		Raw:            rawReq.Raw,
	}

	// 参考图先上传换 file_id。
	uploaded := make([]map[string]any, 0, len(request.Images))
	for _, image := range request.Images {
		payload, decodeErr := decodeBase64Image(image[0])
		if decodeErr != nil {
			continue
		}
		if request.Progress != nil {
			request.Progress("uploading")
		}
		ref, uploadErr := c.UploadImage(payload, image[1], image[2])
		if uploadErr != nil {
			return nil, nil, uploadErr
		}
		uploaded = append(uploaded, ref)
	}

	if request.Progress != nil {
		request.Progress("preparing_conversation")
	}
	conduit, err := c.prepareImageConversation(ctx, request.Prompt, request.Model,
		request.DefaultUpstreamModel, request.DefaultThinkingEffort, req)
	if err != nil {
		return nil, nil, err
	}

	if request.Progress != nil {
		request.Progress("generating")
	}
	stream, err := c.startImageGeneration(ctx, request.Prompt, request.Model,
		request.DefaultUpstreamModel, request.DefaultThinkingEffort, req, conduit, uploaded)
	if err != nil {
		return nil, nil, err
	}

	payloads := make(chan string, 64)
	errs := make(chan error, 1)
	go func() {
		defer close(payloads)
		defer close(errs)
		defer stream.Close()
		if readErr := readSSEPayloads(stream, payloads); readErr != nil {
			errs <- readErr
		}
	}()
	return payloads, errs, nil
}
