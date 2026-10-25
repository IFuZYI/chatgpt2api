package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"

	"github.com/IFuZYI/chatgpt2api/internal/protocol"
)

// CodexResponsesInstructions 逐字取自 services/openai_backend_api.py:108-110。
const CodexResponsesInstructions = "Use the image_generation tool to create exactly one image for the user's request. " +
	"Return the generated image result."

// codexImageToolModel 是 tools[0].model。Python 在 :790 硬编码 "gpt-image-2"，
// 不跟随请求里的模型名（模型名只决定链路，不决定画图模型）。
const codexImageToolModel = "gpt-image-2"

// codexResponsesTimeout 对齐 Python 的 urlopen(timeout=1200)
// （openai_backend_api.py:850）。
//
// 必须有上限：codex 没有轮询阶段可以兜底超时，一旦上游挂着不回包，
// 异步任务会永远停在 running，且没有任何机制能把它推到终态。
const codexResponsesTimeout = 1200 * time.Second

// codexResponsesPath 是 Codex 专用端点。
//
// 与 web 链路的 /backend-api/f/conversation 是两条完全独立的链路：
// codex 走 Responses 协议（tools/tool_choice），web 走对话协议（conduit_token +
// system_hints=["picture_v2"]）。走错端点会 4xx/5xx 或扣错额度。
const codexResponsesPath = "/backend-api/codex/responses"

// codexImageInput 复刻 _codex_image_input（openai_backend_api.py:609-614）。
//
// 已经是 data URL 的图片原样使用，裸 base64 补 "data:image/png;base64," 前缀。
func codexImageInput(prompt string, images []string) []any {
	content := make([]any, 0, len(images)+1)
	content = append(content, map[string]any{"type": "input_text", "text": prompt})
	for _, image := range images {
		payload := image
		if !strings.HasPrefix(payload, "data:image/") {
			payload = "data:image/png;base64," + payload
		}
		content = append(content, map[string]any{"type": "input_image", "image_url": payload})
	}
	return []any{map[string]any{"role": "user", "content": content}}
}

// codexImagePayload 复刻 iter_codex_image_response_events 的请求体（:786-800）。
//
// 键集与取值是上游契约，逐字对齐 Python：
//   - model 固定 CodexResponsesModel（gpt-5.5），不是请求里的 codex-gpt-image-2
//   - action 由「有没有参考图」决定：有图 edit，无图 generate
//   - size/quality 空值时回落到 "1024x1024" / "auto"
func codexImagePayload(prompt string, images []string, size, quality string) map[string]any {
	if strings.TrimSpace(size) == "" {
		size = "1024x1024"
	}
	if strings.TrimSpace(quality) == "" {
		quality = "auto"
	}
	action := "generate"
	if len(images) > 0 {
		action = "edit"
	}
	return map[string]any{
		"model":        CodexResponsesModel,
		"instructions": CodexResponsesInstructions,
		"store":        false,
		"input":        codexImageInput(prompt, images),
		"tools": []any{map[string]any{
			"type":          "image_generation",
			"model":         codexImageToolModel,
			"action":        action,
			"size":          size,
			"quality":       quality,
			"output_format": "png",
		}},
		"tool_choice": map[string]any{"type": "image_generation"},
		"stream":      true,
	}
}

// streamCodexImage 走 Codex Responses 链路并返回 SSE payload 流。
//
// 与 web 链路的关键差异：
//   - 不做 sentinel 握手（chatRequirements），codex 端点只认 Bearer token
//   - 不上传参考图（图片以 data URL 内联进 input）
//   - 结果以内联 base64 出现在 image_generation_call.result 里，没有 file_id
//     可解析，因此不需要轮询/下载
func (c *Client) streamCodexImage(
	ctx context.Context,
	prompt, model string,
	images [][3]string,
	size, quality string,
	onProgress func(string),
) (<-chan string, <-chan error, error) {
	if !protocol.IsCodexImageModel(model) {
		return nil, nil, fmt.Errorf("codex image stream requires a codex image model, got %q", model)
	}
	if strings.TrimSpace(c.accessToken) == "" {
		return nil, nil, fmt.Errorf("codex responses endpoint requires an access token")
	}

	encoded := make([]string, 0, len(images))
	for _, image := range images {
		if strings.TrimSpace(image[0]) == "" {
			continue
		}
		encoded = append(encoded, image[0])
	}

	body, err := json.Marshal(codexImagePayload(prompt, encoded, size, quality))
	if err != nil {
		return nil, nil, err
	}

	// 整个请求（含读完流）都受这个 deadline 约束：fhttp 的请求 ctx 会一直
	// 管到 body 读完，所以 readSSEPayloads 也被它罩住。
	//
	// cancel 不能 defer 在这个函数里 —— 函数返回时流还没读，defer 会当场
	// 取消 ctx、把 body 掐断。交给读流的 goroutine 在结束时调用。
	streamCtx, cancel := context.WithTimeout(ctx, codexResponsesTimeout)

	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost, BaseURL+codexResponsesPath, newBytesReader(body))
	if err != nil {
		cancel()
		return nil, nil, err
	}
	// Python 只发 Authorization + Content-Type；这里复用 c.headers 让 TLS 指纹、
	// UA 与其它链路一致（Authorization 由 accessToken 注入）。
	req.Header = c.headers(codexResponsesPath, map[string]string{
		"Content-Type": "application/json",
		"Accept":       "text/event-stream",
	})

	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if resp.StatusCode >= 400 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		cancel()
		return nil, nil, NewStatusError(resp.StatusCode, retryAfterFromBody(preview),
			fmt.Sprintf("%s failed: status=%d body=%s", codexResponsesPath, resp.StatusCode, truncate(string(preview), 300)))
	}

	if onProgress != nil {
		onProgress("generating")
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	payloads := make(chan string, 64)
	errs := make(chan error, 1)
	go func() {
		defer cancel()
		defer close(payloads)
		defer close(errs)
		defer resp.Body.Close()

		// 非流式 JSON：Python 的 _iter_codex_response_events 同样把整个 body
		// 当成一个事件（content-type 含 application/json 的分支）。
		if strings.Contains(contentType, "application/json") {
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
			if readErr != nil {
				errs <- readErr
				return
			}
			if trimmed := strings.TrimSpace(string(raw)); trimmed != "" {
				payloads <- trimmed
			}
			return
		}
		if readErr := readSSEPayloads(resp.Body, payloads); readErr != nil {
			errs <- readErr
		}
	}()
	return payloads, errs, nil
}
