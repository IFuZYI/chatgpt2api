package protocol

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"
)

// ImageGenerator 把「一次图片请求」跑成 ImageOutput 序列。
//
// 它对应 Python 的 stream_image_outputs，但把其中嵌套的重试逻辑拆成了
// 清晰的阶段：SSE 消费 → 结果判定 → 轮询 → 下载。行为保持一致：
//   - 先 yield progress（前端进度条要用）
//   - 有图 → yield result
//   - 无图但有 message → yield message（不抛错，让上层决定）
//   - 异常 → 返回 error
type ImageGenerator struct {
	Upstream ImageUpstream
	Config   PollOptions
	// SaveImage 把图片字节落盘并返回公开 URL（nil 表示不落盘）。
	SaveImage func(payload []byte, baseURL string) (string, error)
	// OnProgress 是进度回调（写任务状态用）。
	OnProgress func(step string)
	// OnResult 在真的产出图片时调用一次（用于判定是否该扣额度）。
	OnResult func()
}

// Generate 执行一次图片生成。返回的 outputs 已按顺序排好。
func (g *ImageGenerator) Generate(ctx context.Context, request ImageRequest) ([]ImageOutput, error) {
	index, total := 1, 1
	outputs := make([]ImageOutput, 0, 8)

	if request.Model == "" {
		request.Model = "gpt-image-2"
	}
	if !IsSupportedImageModel(request.Model) {
		return outputs, &ImageGenerationError{
			Message:    "unsupported image model,supported models: " + strings.Join(sortedImageModels(), ", "),
			StatusCode: 400,
			ErrorType:  "invalid_request_error",
			Code:       "model_not_found",
		}
	}

	// codex 模型走 /backend-api/codex/responses，是另一套协议：
	// 图片以 base64 内联返回，没有 conversation/file_id，因此不需要轮询、
	// 下载与对话清理。走 web 链路会 4xx/5xx 或扣错额度（D3-C4）。
	if IsCodexImageModel(request.Model) {
		return g.generateCodexImage(ctx, request)
	}

	conversationID, message, fileIDs, sedimentIDs, blocked, turnUseCase, err :=
		g.consumeStream(ctx, request, &outputs, index, total)
	if err != nil {
		return outputs, err
	}

	if g.OnProgress != nil {
		g.OnProgress("image_stream_resolve_start")
	}

	// 被 moderation 拦下：拿结构化错误或直接返回 message。
	if message != "" && len(fileIDs) == 0 && len(sedimentIDs) == 0 && blocked {
		detailed := g.detailedTaskError(ctx, conversationID)
		text := detailed
		if text == "" {
			text = message
		}
		if text == "" {
			text = "Image generation was rejected by upstream policy."
		}
		if request.MessageAsError {
			// 同步接口（/v1/images/generations 与 /edits）无条件传 true：
			// 上游拒图必须变成 4xx，而不是 200 + 空 data。
			// 异步任务传 false（它要把失败原因记进任务状态）。
			g.cleanupConversation(conversationID, false)
			return outputs, &ImageGenerationError{
				Message:        text,
				StatusCode:     http.StatusBadRequest,
				ErrorType:      "invalid_request_error",
				Code:           "content_policy_violation",
				ConversationID: conversationID,
			}
		}
		outputs = append(outputs, ImageOutput{
			Kind: ImageKindMessage, Model: request.Model, Index: index, Total: total,
			Text: text, ConversationID: conversationID,
		})
		g.cleanupConversation(conversationID, false)
		return outputs, nil
	}

	// 判断是否该继续轮询：带参考图，或上游标记 turn_use_case=image gen。
	shouldPoll := len(request.Images) > 0 || turnUseCase == "image gen"

	if message != "" && len(fileIDs) == 0 && len(sedimentIDs) == 0 && !shouldPoll {
		if request.MessageAsError {
			g.cleanupConversation(conversationID, false)
			return outputs, &ImageGenerationError{
				Message:        message,
				StatusCode:     http.StatusBadRequest,
				ErrorType:      "invalid_request_error",
				Code:           "content_policy_violation",
				ConversationID: conversationID,
			}
		}
		outputs = append(outputs, ImageOutput{
			Kind: ImageKindMessage, Model: request.Model, Index: index, Total: total,
			Text: message, ConversationID: conversationID,
		})
		g.cleanupConversation(conversationID, false)
		return outputs, nil
	}

	isTextReply := message != "" && IsModelTextReplyInsteadOfImage(message)

	// conversation_id 丢失时尝试找回（SSE 太短会捕获不到）。
	if (isTextReply || shouldPoll) && conversationID == "" {
		if recovered, findErr := g.Upstream.FindConversationByPrompt(ctx, request.Prompt, time.Now()); findErr == nil {
			conversationID = recovered
		}
	}

	// 轮询前先查一次 tasks，但只在「不轮询且不是文本回复」时才据此提前失败。
	if len(fileIDs) == 0 && len(sedimentIDs) == 0 && conversationID != "" {
		detailed := g.detailedTaskError(ctx, conversationID)
		if detailed != "" && !shouldPoll && !isTextReply {
			outputs = append(outputs, ImageOutput{
				Kind: ImageKindMessage, Model: request.Model, Index: index, Total: total,
				Text: detailed, ConversationID: conversationID,
			})
			g.cleanupConversation(conversationID, false)
			return outputs, nil
		}
	}

	if len(fileIDs) == 0 && len(sedimentIDs) == 0 && conversationID != "" {
		pollOptions := g.Config
		// 文本回复场景图片可能还在异步生成，给更长的预算。
		if isTextReply && pollOptions.TimeoutSecs < 300 {
			pollOptions.TimeoutSecs = 300
		}

		result, pollErr := PollImageResults(ctx, g.Upstream, conversationID, pollOptions, fileIDs, sedimentIDs)
		if pollErr != nil {
			if isTextReply {
				// 文本回复场景下，首次轮询失败不该直接中断：图片可能还在生成。
				fileIDs, sedimentIDs = result.FileIDs, result.SedimentIDs
			} else {
				g.cleanupConversation(conversationID, false)
				return outputs, wrapImageError(pollErr, request.Model, conversationID)
			}
		} else {
			fileIDs, sedimentIDs = result.FileIDs, result.SedimentIDs
		}
	}

	// 拿到 id → 下载 → 落盘 → result。
	if len(fileIDs) > 0 || len(sedimentIDs) > 0 {
		if outputs, ok := g.emitResult(ctx, request, outputs, index, total, conversationID, fileIDs, sedimentIDs); ok {
			return outputs, nil
		}
	}

	if message != "" {
		outputs = append(outputs, ImageOutput{
			Kind: ImageKindMessage, Model: request.Model, Index: index, Total: total,
			Text: message, ConversationID: conversationID,
		})
		g.cleanupConversation(conversationID, false)
		return outputs, nil
	}

	// 兜底：既没图也没 message，给一条可操作的提示而不是模糊报错。
	fallback := "Image generation started upstream but the response was incomplete. Please try again."
	if shouldPoll {
		fallback = "Image generation completed upstream but the result could not be retrieved. " +
			"The image may still be processing. Please try again in a moment."
	}
	outputs = append(outputs, ImageOutput{
		Kind: ImageKindMessage, Model: request.Model, Index: index, Total: total,
		Text: fallback, ConversationID: conversationID,
	})
	g.cleanupConversation(conversationID, false)
	return outputs, nil
}

// consumeStream 消费 SSE 流，产出 progress，并返回最终状态。
func (g *ImageGenerator) consumeStream(
	ctx context.Context,
	request ImageRequest,
	outputs *[]ImageOutput,
	index, total int,
) (conversationID, message string, fileIDs, sedimentIDs []string, blocked bool, turnUseCase string, err error) {
	imageRequest := request
	// 图片链路要把尺寸/质量写进提示词（上游按提示词理解，不是参数）。
	imageRequest.Prompt = promptWithImageHints(request.Prompt, request.Size, request.Quality)
	// 再叠加全局系统提示词（Python conversation.py:697 的顺序：
	// prompt_with_global_system(build_image_prompt(...))）。
	imageRequest.Prompt = PromptWithGlobalSystem(imageRequest.Prompt, request.GlobalSystemPrompt)

	// 图片链路优先走 /backend-api/f/conversation 系列（prepare → generate）。
	//
	// 文本端点 /backend-api/conversation 在图片场景下会被上游拒绝，
	// 因为图片需要 conduit_token 与 system_hints=["picture_v2"]。
	payloads, errs, streamErr := g.Upstream.StreamImage(ctx, ImageStreamRequest{
		Prompt:                imageRequest.Prompt,
		Model:                 request.Model,
		Images:                imageRequest.Images,
		DefaultUpstreamModel:  request.DefaultUpstreamModel,
		DefaultThinkingEffort: request.DefaultThinkingEffort,
		Progress: func(step string) {
			if g.OnProgress != nil {
				g.OnProgress(step)
			}
		},
	})
	if streamErr != nil {
		return "", "", nil, nil, false, "", wrapImageError(streamErr, request.Model, "")
	}

	state := &ConversationState{}
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

		nextRaw := AssistantRawText(event, state.RawText, "")
		nextText := SanitizeOutputText(nextRaw)
		if nextText != state.Text {
			delta := nextText
			if strings.HasPrefix(nextText, state.Text) {
				delta = nextText[len(state.Text):]
			}
			state.RawText = nextRaw
			state.Text = nextText
			if delta != "" {
				*outputs = append(*outputs, ImageOutput{
					Kind: ImageKindProgress, Model: request.Model, Index: index, Total: total,
					Text: delta, UpstreamEventType: "conversation.delta",
					ConversationID: state.ConversationID,
				})
			}
			continue
		}
		state.RawText = nextRaw

		rawType := ""
		if raw, ok := event["type"].(string); ok {
			rawType = raw
		}
		*outputs = append(*outputs, ImageOutput{
			Kind: ImageKindProgress, Model: request.Model, Index: index, Total: total,
			UpstreamEventType: rawType, ConversationID: state.ConversationID,
		})
	}

	for streamErr := range errs {
		if streamErr != nil {
			return "", "", nil, nil, false, "", wrapImageError(streamErr, request.Model, state.ConversationID)
		}
	}

	turnUseCase = state.TurnUseCase
	// 诊断日志：图片链路最容易出问题的地方是「流读完了但 file_id 没提取到」。
	// 保留这行能在生产日志里立刻看出是上游没出图还是我们漏解析。
	debugf("[image] stream done conv=%q files=%v sediment=%v blocked=%v use_case=%q text_len=%d",
		state.ConversationID, state.FileIDs, state.SedimentIDs, state.Blocked, turnUseCase, len(state.Text))
	return state.ConversationID, strings.TrimSpace(state.Text),
		state.FileIDs, state.SedimentIDs, state.Blocked, turnUseCase, nil
}

// emitResult 下载图片并产出 result。返回 (outputs, true) 表示成功。
func (g *ImageGenerator) emitResult(
	ctx context.Context,
	request ImageRequest,
	outputs []ImageOutput,
	index, total int,
	conversationID string,
	fileIDs, sedimentIDs []string,
) ([]ImageOutput, bool) {
	urls, err := g.Upstream.ResolveImageURLs(ctx, conversationID, fileIDs, sedimentIDs)
	if err != nil || len(urls) == 0 {
		debugf("[image] resolve urls failed: err=%v urls=%d conv=%q files=%v sediment=%v",
			err, len(urls), conversationID, fileIDs, sedimentIDs)
		return outputs, false
	}
	debugf("[image] resolved %d urls", len(urls))
	if g.OnProgress != nil {
		g.OnProgress("receiving_image")
	}

	items := make([]map[string]any, 0, len(urls))
	for _, url := range urls {
		payload, downloadErr := g.Upstream.DownloadImage(ctx, url)
		if downloadErr != nil || len(payload) == 0 {
			continue
		}
		items = append(items, map[string]any{
			"b64_json": base64.StdEncoding.EncodeToString(payload),
		})
	}
	if len(items) == 0 {
		debugf("[image] download produced 0 items from %d urls", len(urls))
		return outputs, false
	}

	result := FormatImageResult(items, request.Prompt, request.ResponseFormat,
		g.SaveImage, request.BaseURL, time.Now().Unix(), "")
	data, _ := result["data"].([]any)
	if len(data) == 0 {
		debugf("[image] format produced 0 data entries (items=%d fmt=%q)", len(items), request.ResponseFormat)
		return outputs, false
	}

	converted := make([]map[string]any, 0, len(data))
	for _, item := range data {
		if entry, ok := item.(map[string]any); ok {
			converted = append(converted, entry)
		}
	}
	outputs = append(outputs, ImageOutput{
		Kind: ImageKindResult, Model: request.Model, Index: index, Total: total,
		Data: converted, ConversationID: conversationID,
	})
	if g.OnResult != nil {
		g.OnResult()
	}
	g.cleanupConversation(conversationID, true)
	return outputs, true
}

// detailedTaskError 查 tasks 拿结构化错误文案。
func (g *ImageGenerator) detailedTaskError(ctx context.Context, conversationID string) string {
	if conversationID == "" {
		return ""
	}
	tasks, err := g.Upstream.QueryTasks(ctx, conversationID)
	if err != nil {
		return ""
	}
	for _, task := range tasks {
		if message := TaskErrorMessage(task); message != "" {
			return message
		}
	}
	return ""
}

// cleanupConversation 按配置异步删除上游对话（避免在用户机器上留痕）。
func (g *ImageGenerator) cleanupConversation(conversationID string, success bool) {
	if conversationID == "" || g.Config.RemoveConversation == nil {
		return
	}
	g.Config.RemoveConversation(conversationID, success)
}

// wrapImageError 把底层错误包装成 ImageGenerationError，保留 conversation_id。
func wrapImageError(err error, model, conversationID string) error {
	var generationErr *ImageGenerationError
	if errors.As(err, &generationErr) {
		if generationErr.ConversationID == "" {
			generationErr.ConversationID = conversationID
		}
		return generationErr
	}
	var policyErr *ImageContentPolicyError
	if errors.As(err, &policyErr) {
		return &ImageGenerationError{
			Message:        policyErr.Message,
			StatusCode:     502,
			ErrorType:      "server_error",
			Code:           "content_policy",
			ConversationID: conversationID,
		}
	}
	var timeoutErr *ImagePollTimeoutError
	if errors.As(err, &timeoutErr) {
		return &ImageGenerationError{
			Message:        timeoutErr.Message,
			StatusCode:     504,
			ErrorType:      "server_error",
			Code:           "image_poll_timeout",
			ConversationID: conversationID,
		}
	}
	return &ImageGenerationError{
		Message:        ImageStreamErrorMessage(err.Error()),
		StatusCode:     502,
		ErrorType:      "server_error",
		Code:           "upstream_error",
		ConversationID: conversationID,
	}
}

// promptWithImageHints 复刻 build_image_prompt：把尺寸/质量提示拼进提示词。
func promptWithImageHints(prompt string, size any, quality string) string {
	hints := make([]string, 0, 2)
	if text := strings.TrimSpace(stringOf(size)); text != "" {
		hints = append(hints, "输出图片尺寸为 "+text+"。")
	}
	if strings.TrimSpace(quality) != "" {
		hints = append(hints, "输出图片质量为 "+quality+"。")
	}
	if len(hints) == 0 {
		return prompt
	}
	return strings.TrimSpace(prompt) + "\n\n" + strings.Join(hints, "")
}

// sortedImageModels 返回排序后的受支持图片模型（用于错误文案）。
func sortedImageModels() []string {
	names := make([]string, 0, len(ImageModels))
	for name := range ImageModels {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}
