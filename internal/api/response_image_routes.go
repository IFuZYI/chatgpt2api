package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/gin-gonic/gin"
)

// 本文件实现 /v1/responses 的图片生成工具形态（Python 权威实现：
// services/protocol/openai_v1_response.py:413-446 的 response_events 图片分支 +
// :449-453 的 handle）。
//
// 与文本分支的分流在 remaining_routes.go：请求带 image_generation 工具就走这里。

// handleImageResponses 复刻 response_events 的图片分支：
//
//	prompt 为空 → 400
//	model 缺省 gpt-image-2
//	参考图 → encode_images（只保留解出字节的那一张）
//	input_image_tokens 按 input 里的图算
//	tool.size / tool.quality 透传给生图链路
//
// 流式与非流式共用同一条事件序列：非流式把事件收集起来，只回
// response.completed 里的 response 对象（Python 的 collect_response）。
func (d *Deps) handleImageResponses(c *gin.Context, body map[string]any) {
	prompt := protocol.ExtractResponsePrompt(body["input"])
	if prompt == "" {
		writeErr(c, Errorf(http.StatusBadRequest, "input text is required"))
		return
	}

	// 内容检查必须在取号之前：命中敏感词不该消耗任何账号额度。
	if d.guardPrompt(c, prompt) {
		return
	}

	model := orDefaultValue(strings.TrimSpace(stringValueOf(body["model"])), "gpt-image-2")

	// 参考图：extract_response_image 只取第一张，与 Python 一致。
	images, imageErr := responseImageInput(body["input"])
	if imageErr != nil {
		writeErr(c, imageErr)
		return
	}
	inputImageTokens := protocol.CountResponseImageInputTokens(body["input"], model)

	tool := protocol.ResponseImageTool(body)
	request := protocol.ImageRequest{
		Prompt: prompt,
		Model:  model,
		// Python 的 response_events 不读 body 的 n：ConversationRequest 的 n 走
		// 默认值 1，所以这条链路永远只生成一张图（与 chat 形态不同）。
		N:              1,
		Size:           stringValueOf(tool["size"]),
		Quality:        orDefaultValue(stringValueOf(tool["quality"]), "auto"),
		ResponseFormat: "b64_json",
		BaseURL:        resolveImageBaseURL(c, d),
		Images:         images,
		// Python 的图片分支不传 message_as_error（默认 False）：
		// 上游拒图要以 message 的形式出现在事件里，而不是变成 4xx。
		MessageAsError:        false,
		GlobalSystemPrompt:    d.Config.GlobalSystemPrompt(),
		DefaultUpstreamModel:  d.Config.DefaultUpstreamModelName(),
		DefaultThinkingEffort: d.Config.DefaultThinkingEffort(),
	}

	run := d.imageResponseOutputs(c.Request.Context(), request)

	if truthyValue(body["stream"]) {
		d.streamImageResponses(c, run, prompt, model, inputImageTokens, tool["size"], request.Quality)
		return
	}

	events := make([]map[string]any, 0, 8)
	collect := func(event map[string]any) error {
		events = append(events, event)
		return nil
	}
	err := resolveImageResponseError(
		protocol.StreamImageResponse(run.outputs, prompt, model, inputImageTokens, tool["size"], request.Quality, collect),
		run.Finish(),
	)
	if err != nil {
		writeErr(c, mapImageError(err))
		return
	}
	response, err := protocol.CollectResponse(events)
	if err != nil {
		writeErr(c, mapImageError(err))
		return
	}
	c.JSON(http.StatusOK, response)
}

// resolveImageResponseError 决定最终对外报哪个错误。
//
// 对齐 Python 的异常传播路径：池子自己抛的错（ImageGenerationError / 号池错误）
// 会穿过 stream_image_response 的 `for output in image_outputs` 冒出来，优先于
// 兜底的 RuntimeError("image generation failed")。反过来，只要事件流已经正常
// 产出了 completed，池子随后的报错在 Python 里也就被丢掉了（生成器已经 return，
// 调用方不再拉取）—— 这里同样不覆盖已完成的结果。
func resolveImageResponseError(streamErr error, poolErr error) error {
	if streamErr == nil {
		return nil
	}
	if poolErr != nil && errors.Is(streamErr, protocol.ErrImageResponseGenerationFailed) {
		return poolErr
	}
	return streamErr
}

// streamImageResponses 把事件序列边产边写（SSE）。
//
// 生图要几十秒：攒完再写会被 nginx/Cloudflare 的 60s 空闲超时切断
// （与 serveImageRequest 的流式分支、streamImageChat 同一个理由）。
// Python 也是这个顺序 —— response.created 立刻发出，每张图落地就补一条
// output_item.done。
func (d *Deps) streamImageResponses(
	c *gin.Context,
	run *imageResponseRun,
	prompt, model string,
	inputImageTokens int,
	size any,
	quality string,
) {
	started := false
	write := func(event map[string]any) error {
		if !started {
			started = true
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")
			c.Status(http.StatusOK)
			_, _ = c.Writer.WriteString(": stream-open\n\n")
		}
		payload, err := marshalJSON(event)
		if err != nil {
			return nil
		}
		_, _ = c.Writer.WriteString("data: " + payload + "\n\n")
		c.Writer.Flush()
		return nil
	}

	streamErr := protocol.StreamImageResponse(run.outputs, prompt, model, inputImageTokens, size, quality, write)
	streamErr = resolveImageResponseError(streamErr, run.Finish())
	if streamErr != nil {
		// 整条流没产出可用输出。Python 的异常同样是从生成器里抛出来、
		// 被 sse_json_stream 兜住：头已经发出去了（response.created 一定在），
		// 所以只能补一条 error 事件再结束 —— 客户端不会把半截流当正常完成。
		payload, marshalErr := marshalJSON(map[string]any{
			"error": map[string]any{"message": streamErr.Error(), "type": "server_error"},
		})
		if marshalErr == nil {
			_, _ = c.Writer.WriteString("data: " + payload + "\n\n")
		}
	}
	_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	c.Writer.Flush()
}

// imageResponseRun 是一次生图池的产出通道 + 收尾句柄。
//
// 为什么要 Stop：StreamImageResponse 在拿到第一条 result/message 后就会返回
// （Python 的相同行为），此时池子可能还在产 output。若不通知它停，池子会卡在
// 「往 channel 里塞 output」上，永远走不到写失败结果那一步 —— 那个 goroutine
// 和它的账号槽位就都泄漏了。
type imageResponseRun struct {
	outputs  chan protocol.ImageOutput
	failures chan error
	stop     chan struct{}
	stopOnce sync.Once
}

// Stop 通知池子停止投递（幂等）。
func (r *imageResponseRun) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
}

// Finish 停掉投递并取回池子的最终结果（nil 表示至少产出一条 output）。
//
// 必须在 Stop 之后调用：failures 只在池子真正结束后才有值。
func (r *imageResponseRun) Finish() error {
	r.Stop()
	return <-r.failures
}

// imageResponseOutputs 在后台跑生图池，按完成顺序把 output 推给消费者。
//
// channel 带缓冲（每张图按 3 条 output 估）：生成 goroutine 不该被慢客户端
// 拖住号池槽位的周转；缓冲塞满后靠 stop 兜底，不会死锁。
func (d *Deps) imageResponseOutputs(ctx context.Context, request protocol.ImageRequest) *imageResponseRun {
	capacity := request.N * 3
	if capacity < 3 {
		capacity = 3
	}
	run := &imageResponseRun{
		outputs:  make(chan protocol.ImageOutput, capacity),
		failures: make(chan error, 1),
		stop:     make(chan struct{}),
	}

	pool := &protocol.ImagePool{
		Parallel: d.Config.ImageParallelGeneration(),
		Acquire: func(ctx context.Context, index int) (*protocol.ImageGenerator, func(), error) {
			// 同步链路不需要 token 回写（没有 resume-poll）。
			generator, release, _, err := d.acquireImageGenerator(ctx, request.Model, index)
			return generator, release, err
		},
	}

	goSafe(d.Logs, "响应图片", func() {
		defer close(run.outputs)
		defer close(run.failures)

		result, err := pool.Run(ctx, request)
		run.failures <- err
		if err != nil {
			// Python 的池子在抛异常时不会 yield 任何 output（异常直接穿过
			// stream_image_outputs_with_pool）：部分进度/拒图文案不能在这条
			// 路径上被当成「成功的文本回复」。
			return
		}
		for _, output := range result.Outputs {
			select {
			case run.outputs <- output:
			case <-run.stop:
				return
			}
		}
	})

	return run
}

// responseImageInput 复刻 Python 的
// `extract_response_image(input)` + `encode_images([(data, "image.png", mime)])`。
//
// encode_images 只保留解出字节的条目，所以「没有参考图」与「解出空图」都是 nil。
func responseImageInput(inputValue any) ([][3]string, *HTTPError) {
	image, err := protocol.ExtractResponseImage(inputValue)
	if err != nil {
		// protocol 层把 Python 的 HTTPException(400) 变成 error：解码失败是 400。
		var chatErr *protocol.ChatImageError
		if errors.As(err, &chatErr) {
			return nil, Errorf(http.StatusBadRequest, chatErr.Message)
		}
		return nil, Errorf(http.StatusBadRequest, err.Error())
	}
	if image == nil || len(image.Data) == 0 {
		return nil, nil
	}
	return [][3]string{{
		base64.StdEncoding.EncodeToString(image.Data),
		"image.png",
		image.MIMEType,
	}}, nil
}
