package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ImageUpstreamFactory 为一次图片请求构造带轮询能力的上游客户端。
type ImageUpstreamFactory func(ctx context.Context, accessToken string) (protocol.ImageUpstream, func(), error)

// pollOptions 把配置转成轮询参数。
func (d *Deps) pollOptions(accessToken string) protocol.PollOptions {
	return protocol.PollOptions{
		TimeoutSecs:     float64(d.Config.ImagePollTimeoutSecs()),
		IntervalSecs:    d.Config.ImagePollIntervalSecs(),
		InitialWaitSecs: d.Config.ImagePollInitialWaitSecs(),
		SettleEnabled:   d.Config.ImageSettleEnabled(),
		CheckBeforeHit:  d.Config.ImageCheckBeforeHitEnabled(),
		SettleSecs:      d.Config.ImageSettleSecs(),
		RemoveConversation: func(conversationID string, success bool) {
			// 必须带上生成时那个账号的 token：Python 用的是已认证的
			// backend 实例（conversation.py:826），空 token 会被上游 401，
			// 清理永远失败。
			d.removeConversationLater(accessToken, conversationID, success)
		},
	}
}

// removeConversationLater 复刻 _remove_image_conversation_later：
// 按配置异步删除上游对话（失败只记日志）。
//
// 注意：这里刻意用 context.WithoutCancel —— 客户端可能已经断开，
// 但清理是「用户走了更需要做」的动作（ADR-0007）。
func (d *Deps) removeConversationLater(accessToken, conversationID string, success bool) {
	removeAfterResult := d.Config.ImageRemoveConversationAfterResult()
	removeAlways := d.Config.ImageRemoveConversationAlways()
	if !removeAlways && !(success && removeAfterResult) {
		return
	}
	if conversationID == "" || d.ImageUpstream == nil || accessToken == "" {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		upstream, closeFn, err := d.ImageUpstream(ctx, accessToken)
		if err != nil {
			return
		}
		defer closeFn()
		_ = upstream.DeleteConversation(ctx, conversationID)
	}()
}

// registerImageGeneration 挂载 /v1/images/generations 与 /v1/images/edits。
func (d *Deps) registerImageGeneration(r *gin.Engine) {
	r.POST("/v1/images/generations", func(c *gin.Context) {
		if _, authErr := d.RequireIdentityCtx(c); authErr != nil {
			writeErr(c, authErr)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}

		prompt := strings.TrimSpace(stringValueOf(body["prompt"]))
		if prompt == "" {
			writeErr(c, Errorf(http.StatusBadRequest, "prompt is required"))
			return
		}
		// 内容检查必须在取号之前：命中敏感词不该消耗任何账号额度。
		if d.guardPrompt(c, prompt) {
			return
		}
		// n 越界必须报错，不能静默夹取（Python 是 pydantic ge=1/le=4 → 422）。
		// 夹取会让用户以为 n=5 生效了，实际只出 4 张。
		n, nErr := requireImageCount(body["n"], true)
		if nErr != nil {
			writeErr(c, nErr)
			return
		}
		request := protocol.ImageRequest{
			Prompt:         prompt,
			Model:          orDefaultValue(stringValueOf(body["model"]), "gpt-image-2"),
			N:              n,
			Size:           stringValueOf(body["size"]),
			Quality:        orDefaultValue(stringValueOf(body["quality"]), "auto"),
			ResponseFormat: orDefaultValue(stringValueOf(body["response_format"]), "b64_json"),
			BaseURL:        resolveImageBaseURL(c, d),
		}
		d.serveImageRequest(c, request, truthyValue(body["stream"]), nil)
	})

	r.POST("/v1/images/edits", func(c *gin.Context) {
		if _, authErr := d.RequireIdentityCtx(c); authErr != nil {
			writeErr(c, authErr)
			return
		}
		// 编辑接口同时支持 multipart 与 JSON（官方两种形态）。
		payload, images, mask, parseErr := parseImageEditRequest(c)
		if parseErr != nil {
			writeErr(c, parseErr)
			return
		}
		if d.guardPrompt(c, strings.TrimSpace(stringValueOf(payload["prompt"]))) {
			return
		}
		// 与 /v1/images/generations 保持同一套校验：越界报错而非静默夹取。
		// Python 的 _parse_count 对 edits 返回 400，所以 strictPydantic=false。
		editN, editNErr := requireImageCount(payload["n"], false)
		if editNErr != nil {
			writeErr(c, editNErr)
			return
		}
		request := protocol.ImageRequest{
			Prompt:         strings.TrimSpace(stringValueOf(payload["prompt"])),
			Model:          orDefaultValue(stringValueOf(payload["model"]), "gpt-image-2"),
			N:              editN,
			Size:           stringValueOf(payload["size"]),
			Quality:        orDefaultValue(stringValueOf(payload["quality"]), "auto"),
			ResponseFormat: orDefaultValue(stringValueOf(payload["response_format"]), "b64_json"),
			BaseURL:        resolveImageBaseURL(c, d),
			Images:         images,
		}
		if request.Prompt == "" {
			writeErr(c, Errorf(http.StatusBadRequest, "prompt is required"))
			return
		}
		if len(request.Images) == 0 {
			writeErr(c, Errorf(http.StatusBadRequest, "image file or image_url is required"))
			return
		}
		// 局部重绘：mask 的合成放进 serveImageRequest，这样 call 日志能覆盖
		// 合成失败的路径（Python 用 LoggedCall 包住整个 handler）。
		d.serveImageRequest(c, request, truthyValue(payload["stream"]), mask)
	})
}

// serveImageRequest 是生成与编辑的共同出口。
//
// masks 非空时先做局部重绘合成（Python openai_v1_image_edit.py:53 在构造
// ConversationRequest 之前做同一件事），再送上游。
func (d *Deps) serveImageRequest(c *gin.Context, request protocol.ImageRequest, stream bool, masks [][3]string) {
	call := d.beginCall("生图", c.FullPath(), request.Model, c.GetString("key_name"))
	// 失败路径自己调 call.finish(err, ...) 记失败，defer 只兜成功路径，
	// 否则一次请求会写两条日志。
	logged := false
	defer func() {
		if logged {
			return
		}
		call.finish(nil, "", nil)
	}()

	// 局部重绘：把 mask 的 alpha 合成进图片再送上游。
	//
	// 必须在 beginCall 之后做：mask 解码失败也要进 call 日志（Python 的
	// LoggedCall 包住整个 handler，_composite_mask 抛异常同样被记录）。
	// usage 也按合成后的图算 —— Python 也是先合成再计数。
	if len(masks) > 0 {
		composited, maskErr := protocol.CompositeMask(request.Images, masks)
		if maskErr != nil {
			logged = true
			call.finish(maskErr, "", nil)
			writeErr(c, mapImageError(maskErr))
			return
		}
		request.Images = composited
	}

	// 同步接口：上游拒图必须变成 4xx（Python openai_v1_image_generations.py:32
	// 与 openai_v1_image_edit.py:75 都无条件传 message_as_error=True）。
	request.MessageAsError = true
	// 全局系统提示词必须从配置注入：protocol 层不 import config。
	request.GlobalSystemPrompt = d.Config.GlobalSystemPrompt()
	request.DefaultUpstreamModel = d.Config.DefaultUpstreamModelName()
	request.DefaultThinkingEffort = d.Config.DefaultThinkingEffort()

	// 流式：边生成边把进度 chunk 推给客户端。
	//
	// 之前是 pool.Run 跑完再一次性写：生图要几十秒，期间客户端收不到
	// 任何字节，nginx/Cloudflare 的 60s 空闲超时会切断连接。
	//
	// 用 buffered channel 而不是无缓冲：生成 goroutine 不能被慢客户端
	// 阻塞（否则会拖慢整个号池的槽位周转）。容量按进度事件量级给 64。
	var progressChunks chan map[string]any
	if stream {
		progressChunks = make(chan map[string]any, 64)
	}

	pool := &protocol.ImagePool{
		Parallel: d.Config.ImageParallelGeneration(),
		Acquire: func(ctx context.Context, index int) (*protocol.ImageGenerator, func(), error) {
			// 同步链路不需要 token 回写（没有 resume-poll），丢弃第四个返回值。
			generator, release, _, err := d.acquireImageGenerator(ctx, request.Model, index)
			return generator, release, err
		},
	}
	if stream {
		acquire := pool.Acquire
		pool.Acquire = func(ctx context.Context, index int) (*protocol.ImageGenerator, func(), error) {
			generator, release, err := acquire(ctx, index)
			if err != nil {
				return generator, release, err
			}
			// 把上游进度事件转成 chunk 推进 channel。非阻塞发送：
			// 客户端读得慢时丢弃进度而不是卡住生成。
			generator.OnProgress = func(step string) {
				select {
				case progressChunks <- protocol.ImageOutput{
					Kind:  protocol.ImageKindProgress,
					Model: request.Model,
					Index: index,
					Total: request.N,
					Text:  step,
				}.ToChunk():
				default:
				}
			}
			return generator, release, err
		}
	}

	// 流式时生成在后台跑，主协程边读 channel 边写；非流式仍然阻塞等待。
	if stream {
		var result protocol.PoolResult
		var runErr error
		done := make(chan struct{})

		// 生成结束后把「结果 chunk」也灌进同一条 channel，再关闭。
		// 顺序必须是：进度 → 结果 → [DONE]。先关 channel 再补写结果
		// 会把 [DONE] 夹在中间，客户端解析到 [DONE] 就停了。
		go func() {
			defer close(done)
			result, runErr = pool.Run(c.Request.Context(), request)
			for _, output := range result.Outputs {
				progressChunks <- output.ToChunk()
			}
			close(progressChunks)
		}()

		writeSSEFromChannel(c, progressChunks)
		<-done

		if runErr != nil {
			// 头已经发出去了，改不了状态码，但必须在流里补一条 error 事件：
			// 只写 [DONE] 会让客户端把「0 张图」当成「成功但无输出」，
			// 这是生图接口最糟的失败形态（无法区分空结果与故障）。
			// 与 response_image_routes.go / chat_image_routes.go 的做法对齐。
			message := mapImageError(runErr).Error()
			if payload, marshalErr := marshalJSON(map[string]any{
				"error": map[string]any{
					"message": message,
					"type":    "server_error",
				},
			}); marshalErr == nil {
				_, _ = c.Writer.WriteString("data: " + payload + "\n\n")
				c.Writer.Flush()
			}
			call.finish(runErr, "", nil)
			logged = true
			return
		}
		return
	}

	result, err := pool.Run(c.Request.Context(), request)
	if err != nil {
		writeErr(c, mapImageError(err))
		return
	}

	collected := protocol.CollectImageOutputs(result.Outputs)

	// 非流式：附加 usage，并剔除内部字段。
	collected["usage"] = imageUsageFor(request, collected)
	delete(collected, "_account_email")
	c.JSON(http.StatusOK, collected)
}

// imageSlotOptions 复刻 Python _generate_single_image 的取号条件：
//
//	plan_type, _ := split_image_model(request.model)
//	codex_model := is_codex_image_model(request.model)
//	get_available_access_token(
//	    plan_type=plan_type,
//	    source_type="codex" if codex_model else None,
//	    plan_types=("plus", "team", "pro") if codex_model and not plan_type else None,
//	)
//
// codex 链路必须只挑 source_type=codex 的号：Codex 端点用的是 Codex 那份额度，
// 拿 web 号去打会 401/403，或者扣到 web 额度上。
// 带套餐前缀的别名（team-codex-gpt-image-2）已经指定了套餐，就不再叠加 plan_types。
func imageSlotOptions(model string) service.SlotOptions {
	planType, _ := protocol.SplitImageModel(model)
	if !protocol.IsCodexImageModel(model) {
		return service.SlotOptions{PlanType: planType}
	}
	opts := service.SlotOptions{PlanType: planType, SourceType: service.SourceCodex}
	if planType == "" {
		opts.PlanTypes = []string{"plus", "team", "pro"}
	}
	return opts
}

// acquireImageGenerator 取一个账号槽位并构造 generator。
//
// model 参与取号筛选（codex 模型要挑 codex 来源的号），必须由调用方传入。
func (d *Deps) acquireImageGenerator(ctx context.Context, model string, index int) (*protocol.ImageGenerator, func(), string, error) {
	if d.ImageUpstream == nil {
		return nil, nil, "", protocol.ErrorfImage("image upstream factory is not configured")
	}

	token, err := d.Accts.AcquireImageSlot(ctx, imageSlotOptions(model))
	if err != nil {
		return nil, nil, "", err
	}

	upstream, closeFn, err := d.ImageUpstream(ctx, token)
	if err != nil {
		d.Accts.ReleaseImageSlot(token)
		return nil, nil, "", err
	}

	// succeeded 由 Generate 的调用方在拿到结果后回填：
	// 只有真的产出了图片才该扣额度、记 success。
	// 无条件记 success 会让失败请求也扣额度（实测踩到：假 token 打上游失败，
	// 账号的 success 却 +1、quota 被扣）。
	succeeded := false

	released := false
	release := func() {
		if released {
			return
		}
		released = true
		closeFn()
		d.Accts.MarkImageResult(token, succeeded)
	}

	generator := &protocol.ImageGenerator{
		Upstream: upstream,
		Config:   d.pollOptions(token),
		SaveImage: func(payload []byte, baseURL string) (string, error) {
			_, url, saveErr := d.Images.SaveImageBytes(payload, baseURL)
			return url, saveErr
		},
		OnResult: func() { succeeded = true },
	}
	return generator, release, token, nil
}

// imageUsageFor 计算 usage（图片链路只算输入文本 + 输出图片 token）。
func imageUsageFor(request protocol.ImageRequest, collected map[string]any) map[string]any {
	items := make([]map[string]any, 0)
	if data, ok := collected["data"].([]any); ok {
		for _, item := range data {
			if entry, ok := item.(map[string]any); ok {
				items = append(items, entry)
			}
		}
	}
	return protocol.ImageUsage(
		protocol.CountTextTokens(request.Prompt, request.Model),
		protocol.CountImageInputsTokens(request.Images, request.Model, "auto"),
		protocol.CountImageOutputItemsTokens(items, request.Size, request.Quality),
	)
}

// mapImageError 把图片错误映射成 HTTP 状态与信封。
func mapImageError(err error) *HTTPError {
	if generationErr, ok := err.(*protocol.ImageGenerationError); ok {
		status := generationErr.StatusCode
		if status == 0 {
			status = http.StatusBadGateway
		}
		payload := map[string]any{"error": generationErr.ToOpenAIError()["error"]}
		return ErrorDetail(status, payload)
	}
	message := err.Error()
	if strings.Contains(message, "no available image quota") {
		return ErrorDetail(http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{
				"message": "no available image quota",
				"type":    "insufficient_quota",
				"param":   nil,
				"code":    "insufficient_quota",
			},
		})
	}
	return Errorf(http.StatusBadGateway, message)
}

func orDefaultValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// parseImageEditRequest 解析编辑请求：JSON 或 multipart 两种形态。
//
// 返回 (字段, 图片, mask, 错误)。图片统一成 (base64, filename, mime) 三元组，
// 与 protocol.ImageRequest.Images 的类型一致。
func parseImageEditRequest(c *gin.Context) (map[string]any, [][3]string, [][3]string, *HTTPError) {
	contentType := c.GetHeader("Content-Type")
	if strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			return nil, nil, nil, bodyErr
		}
		images, mask, refErr := imageSourcesFromJSON(body)
		if refErr != nil {
			return nil, nil, nil, refErr
		}
		return body, images, mask, nil
	}

	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		return nil, nil, nil, Errorf(http.StatusBadRequest, "invalid multipart form")
	}

	fields := map[string]any{}
	for _, key := range []string{"prompt", "model", "n", "size", "quality", "response_format", "stream", "client_task_id"} {
		if value := c.PostForm(key); value != "" {
			fields[key] = value
		}
	}

	images := make([][3]string, 0, 2)
	masks := make([][3]string, 0, 1)
	form := c.Request.MultipartForm
	if form != nil {
		for key, files := range form.File {
			target := &images
			if strings.HasPrefix(key, "mask") {
				target = &masks
			}
			for _, header := range files {
				file, openErr := header.Open()
				if openErr != nil {
					continue
				}
				payload := make([]byte, header.Size)
				_, _ = file.Read(payload)
				file.Close()
				mime := header.Header.Get("Content-Type")
				if mime == "" {
					mime = "image/png"
				}
				*target = append(*target, [3]string{
					base64.StdEncoding.EncodeToString(payload),
					header.Filename,
					mime,
				})
			}
		}
	}
	return fields, images, masks, nil
}

// imageSourcesFromJSON 从 JSON body 里取图片引用。
//
// 支持三种：data URL、base64 裸串、以及 {"file_id": ...}（明确拒绝）。
// 远程 URL 拉取在 M6 接入（需要出站客户端）。
func imageSourcesFromJSON(body map[string]any) ([][3]string, [][3]string, *HTTPError) {
	images := make([][3]string, 0, 2)
	masks := make([][3]string, 0, 1)

	collect := func(value any, target *[][3]string) *HTTPError {
		items, ok := value.([]any)
		if !ok {
			if value == nil {
				return nil
			}
			items = []any{value}
		}
		for _, item := range items {
			switch typed := item.(type) {
			case string:
				entry, err := decodeImageString(typed)
				if err != nil {
					return err
				}
				*target = append(*target, entry)
			case map[string]any:
				if stringValueOf(typed["file_id"]) != "" {
					return Errorf(http.StatusBadRequest, "file_id image references are not supported")
				}
				url := stringValueOf(typed["image_url"])
				if url == "" {
					if nested, ok := typed["image_url"].(map[string]any); ok {
						url = stringValueOf(nested["url"])
					}
				}
				if url == "" {
					url = stringValueOf(typed["url"])
				}
				if url == "" {
					continue
				}
				entry, err := decodeImageString(url)
				if err != nil {
					return err
				}
				*target = append(*target, entry)
			}
		}
		return nil
	}

	for _, key := range []string{"image", "images"} {
		if err := collect(body[key], &images); err != nil {
			return nil, nil, err
		}
	}
	if err := collect(body["mask"], &masks); err != nil {
		return nil, nil, err
	}
	return images, masks, nil
}

// decodeImageString 解 data URL 或裸 base64。
func decodeImageString(value string) ([3]string, *HTTPError) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return [3]string{}, Errorf(http.StatusBadRequest, "image is required")
	}
	if strings.HasPrefix(trimmed, "data:") {
		header, payload, found := strings.Cut(trimmed, ",")
		if !found {
			return [3]string{}, Errorf(http.StatusBadRequest, "invalid data url")
		}
		mime := "image/png"
		if parts := strings.SplitN(strings.TrimPrefix(header, "data:"), ";", 2); len(parts) > 0 && parts[0] != "" {
			mime = parts[0]
		}
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return [3]string{}, Errorf(http.StatusBadRequest, "invalid base64 image")
		}
		return [3]string{base64.StdEncoding.EncodeToString(decoded), "image_url.png", mime}, nil
	}
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		// 远程 URL 拉取在 M6 接入。
		return [3]string{}, Errorf(http.StatusBadRequest, "remote image URLs are not supported yet")
	}
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return [3]string{}, Errorf(http.StatusBadRequest, "invalid base64 image")
	}
	return [3]string{base64.StdEncoding.EncodeToString(decoded), "image.png", "image/png"}, nil
}

// requireImageCount 解析并校验 n。
//
// strictPydantic=true 用于 /v1/images/generations：Python 的 pydantic 模型
// `n: int = Field(default=1, ge=1, le=4)` 对越界返回 422 + Pydantic 条目。
// strictPydantic=false 用于编辑链路：Python 的 _parse_count 用
// HTTPException(400, {"error": "n must be between 1 and 4"})。
func requireImageCount(raw any, strictPydantic bool) (int, *HTTPError) {
	const lower, upper = 1, 4

	if raw == nil {
		return 1, nil
	}
	count, ok := parseIntValue(raw)
	if !ok {
		if strictPydantic {
			return 0, ErrorDetail(http.StatusUnprocessableEntity, []any{map[string]any{
				"type":  "int_parsing",
				"loc":   []any{"body", "n"},
				"msg":   "Input should be a valid integer, unable to parse string as an integer",
				"input": raw,
			}})
		}
		return 0, Errorf(http.StatusBadRequest, "n must be an integer")
	}
	if count < lower || count > upper {
		if strictPydantic {
			return 0, rangeValidationError("n", count, lower, upper)
		}
		return 0, Errorf(http.StatusBadRequest, "n must be between 1 and 4")
	}
	return count, nil
}

// parseIntValue 把 JSON 里的数值或数字字符串转成 int。
func parseIntValue(raw any) (int, bool) {
	switch value := raw.(type) {
	case float64:
		if value != float64(int(value)) {
			return 0, false
		}
		return int(value), true
	case int:
		return value, true
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.Atoi(trimmed)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}
