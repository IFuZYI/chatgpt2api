package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/gin-gonic/gin"
)

// chatCallOutcome 是 call 日志的延迟填充结果。
//
// /v1/chat/completions 的日志在 handler 入口就 beginCall、defer finish，
// 但图片形态要到跑完生图才知道账号邮箱与产出 URL。用一个小结构体把这两项
// 从图片链路带回来，避免为了多两个字段再写一条日志（Python 一次请求
// 只对应一个 LoggedCall）。
type chatCallOutcome struct {
	err          error
	accountEmail string
	urls         []string
}

// handleImageChat 复刻 openai_v1_chat_complete.handle 的图片分支：
// 非流式走 image_chat_response，流式走 image_chat_events。
//
// 与 /v1/images/generations 的两处关键差异（都来自 Python）：
//   - 固定 response_format=b64_json：chat 响应要把图内联成 markdown
//   - message_as_error 保持 false：上游拒图要以 message 文本的形式
//     出现在 choices[0].message.content 里，而不是变成 4xx
func (d *Deps) handleImageChat(c *gin.Context, body map[string]any, outcome *chatCallOutcome) {
	model, prompt, n, images, argErr := protocol.ChatImageArgs(body)
	if argErr != nil {
		writeErr(c, mapChatImageArgsError(argErr))
		return
	}

	// 内容检查必须在取号之前：命中敏感词不该消耗任何账号额度。
	// 检查对象是抽取后的 prompt —— 与 Python filter_or_log 的口径一致
	// （它查的是 request_text(prompt, messages) 归一后的文本）。
	if d.guardPrompt(c, prompt) {
		return
	}

	request := protocol.ImageRequest{
		Prompt:         prompt,
		Model:          model,
		N:              n,
		ResponseFormat: "b64_json",
		BaseURL:        resolveImageBaseURL(c, d),
		Images:         images,
		// Python 的 chat_image_args 不传 message_as_error（默认 False）。
		MessageAsError:        false,
		GlobalSystemPrompt:    d.Config.GlobalSystemPrompt(),
		DefaultUpstreamModel:  d.Config.DefaultUpstreamModelName(),
		DefaultThinkingEffort: d.Config.DefaultThinkingEffort(),
	}

	if truthyValue(body["stream"]) {
		d.streamImageChat(c, request, outcome)
		return
	}

	pool := d.imageChatPool(request.Model, outcome)
	result, runErr := pool.Run(c.Request.Context(), request)
	if runErr != nil {
		outcome.err = runErr
		writeErr(c, mapImageError(runErr))
		return
	}

	collected := protocol.CollectImageOutputs(result.Outputs)
	if email := stringValueOf(collected["_account_email"]); email != "" {
		outcome.accountEmail = email
	}
	outcome.urls = imageChatResultURLs(collected)

	c.JSON(http.StatusOK, protocol.ImageChatCompletion(model, collected, prompt, images))
}

// streamImageChat 复刻 image_chat_events + stream_image_chat_completion：
// 边生成边把 chat.completion.chunk 推给客户端。
//
// 进度事件（上游的 bootstrapping / generating / receiving_image 等）在生成
// 过程中就推出去；生图要几十秒，攒到结束再写会被 nginx/Cloudflare 的 60s
// 空闲超时切断（与 serveImageRequest 的流式分支同一个理由）。
func (d *Deps) streamImageChat(c *gin.Context, request protocol.ImageRequest, outcome *chatCallOutcome) {
	outputs := make(chan protocol.ImageOutput, 64)
	var runErr error

	pool := d.imageChatPool(request.Model, outcome)
	acquire := pool.Acquire
	pool.Acquire = func(ctx context.Context, index int) (*protocol.ImageGenerator, func(), error) {
		generator, release, err := acquire(ctx, index)
		if err != nil {
			return generator, release, err
		}
		// 非阻塞发送：客户端读得慢时丢弃进度而不是卡住生成。
		generator.OnProgress = func(step string) {
			select {
			case outputs <- protocol.ImageOutput{
				Kind:  protocol.ImageKindProgress,
				Model: request.Model,
				Index: index,
				Total: request.N,
				Text:  step,
			}:
			default:
			}
		}
		return generator, release, err
	}

	go func() {
		result, err := pool.Run(c.Request.Context(), request)
		runErr = err
		for _, output := range result.Outputs {
			outputs <- output
		}
		close(outputs)
	}()

	stream := protocol.NewChatImageChunkStream(request.Model)
	started := false

	write := func(chunk map[string]any) {
		if !started {
			started = true
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")
			c.Status(http.StatusOK)
			_, _ = c.Writer.WriteString(": stream-open\n\n")
		}
		payload, err := marshalJSON(chunk)
		if err != nil {
			return
		}
		_, _ = c.Writer.WriteString("data: " + payload + "\n\n")
		c.Writer.Flush()
	}

	for output := range outputs {
		if chunk, ok := stream.Chunk(output); ok {
			write(chunk)
		}
	}

	// 失败：还没写任何字节就报正常的 JSON 错误（客户端拿得到状态码与原因）；
	// 已经开流了就只能截断，与 Python 抛异常中断生成器一致 —— 此时不再补
	// stop 块与 [DONE]，否则客户端会把一次失败当成正常结束。
	if runErr != nil {
		outcome.err = runErr
		if !started {
			writeErr(c, mapImageError(runErr))
		}
		return
	}

	for _, chunk := range stream.Finish() {
		write(chunk)
	}
	_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	c.Writer.Flush()
}

// imageChatPool 构造 chat 图片链路用的号池（取号逻辑与生图接口完全一致）。
//
// 顺带记下第一个取到的账号邮箱：Python 的 _generate_single_image 用
// `account_service.get_account(token)` 拿到 email 后挂到每条 output 上
// （conversation.py:1332-1333），collect_image_outputs 取第一个非空值。
// Go 的 ImageGenerator 没有「按 output 挂邮箱」的入口，所以在取号处直接记下，
// 语义等价（同一次请求的第一个账号）。
func (d *Deps) imageChatPool(model string, outcome *chatCallOutcome) *protocol.ImagePool {
	return &protocol.ImagePool{
		Parallel: d.Config.ImageParallelGeneration(),
		Acquire: func(ctx context.Context, index int) (*protocol.ImageGenerator, func(), error) {
			// 同步链路不需要 token 回写（没有 resume-poll），第四个返回值是 token。
			generator, release, token, err := d.acquireImageGenerator(ctx, model, index)
			if err != nil {
				return generator, release, err
			}
			if outcome != nil && outcome.accountEmail == "" && token != "" {
				if account := d.Accts.GetAccount(token); account != nil {
					outcome.accountEmail = strings.TrimSpace(stringValueOf(account["email"]))
				}
			}
			return generator, release, nil
		},
	}
}

// imageChatResultURLs 收集产出 URL 供 call 日志展示。
//
// 对应 Python LoggedCall.log 里的 _collect_urls(result)：结果 dict 里所有
// "url" 字段都会进日志。b64_json 模式下每条 data 同时带 url（图片已落盘）。
func imageChatResultURLs(collected map[string]any) []string {
	data, ok := collected["data"].([]any)
	if !ok {
		return nil
	}
	urls := make([]string, 0, len(data))
	for _, item := range data {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if url := strings.TrimSpace(stringValueOf(entry["url"])); url != "" {
			urls = append(urls, url)
		}
	}
	return urls
}

// mapChatImageArgsError 把 chat 图片链路的参数错误映射成 400。
//
// Python 是 HTTPException(400, {"error": ...})：prompt 缺失、n 越界、
// 参考图解码失败都走这条。
func mapChatImageArgsError(err error) *HTTPError {
	var chatErr *protocol.ChatImageError
	if errors.As(err, &chatErr) {
		return Errorf(http.StatusBadRequest, chatErr.Message)
	}
	return Errorf(http.StatusBadRequest, err.Error())
}
