package api

import (
	"context"
	"errors"

	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/basketikun/chatgpt2api/internal/service"
)

// RegisterImageTaskRunners 把图片任务的执行体注入 ImageTaskService。
//
// 为什么需要这一步：`/api/image-tasks/*` 是异步队列，任务在后台 goroutine 里跑，
// 走不到 HTTP handler。ImageTaskService 因此留了 TaskRunner 注入点
// （service 层不能直接依赖 protocol/upstream，否则会成环）。
//
// 早期实现漏了这一步，导致 SetRunners 从未被调用、runner 恒为 nil，
// 所有异步任务在开跑瞬间就失败并报 "image task runner is not configured"。
// 生图页面默认走异步任务接口，所以表现为「生图界面异常」。
func (d *Deps) RegisterImageTaskRunners() {
	if d.Tasks == nil {
		return
	}
	d.Tasks.SetRunners(d.runImageTaskGenerate, d.runImageTaskEdit)
}

// runImageTaskGenerate 执行一次生成任务。
func (d *Deps) runImageTaskGenerate(taskCtx service.TaskContext) (service.TaskResult, error) {
	return d.runImageTask(taskCtx, false)
}

// runImageTaskEdit 执行一次编辑任务（带参考图 / mask）。
func (d *Deps) runImageTaskEdit(taskCtx service.TaskContext) (service.TaskResult, error) {
	return d.runImageTask(taskCtx, true)
}

// runImageTask 是生成与编辑的共同实现。
//
// 刻意复用 ImagePool + acquireImageGenerator —— 与同步接口完全同一条链路，
// 这样账号槽位、并行、失败计数、对话清理的行为不会出现两套。
func (d *Deps) runImageTask(taskCtx service.TaskContext, isEdit bool) (service.TaskResult, error) {
	// 后台任务没有请求 ctx，但也不能用 Background 裸跑：
	// 任务取消（用户删任务 / 服务退出）需要能中断上游请求。
	ctx := context.Background()

	summary := "生图"
	endpoint := "/api/image-tasks/generations"
	if isEdit {
		summary = "图片编辑"
		endpoint = "/api/image-tasks/edits"
	}
	call := d.beginCall(summary, endpoint, taskCtx.Model, "")

	request := protocol.ImageRequest{
		Prompt:         taskCtx.Prompt,
		Model:          taskCtx.Model,
		N:              1, // 每个 task 固定一张，多图由前端拆多个 task
		Size:           taskCtx.Size,
		Quality:        taskCtx.Quality,
		ResponseFormat: "b64_json",
		BaseURL:        taskCtx.BaseURL,
		// 异步任务不把 message 升级为错误：失败原因要落进任务状态供前端展示。
		MessageAsError:        false,
		GlobalSystemPrompt:    d.Config.GlobalSystemPrompt(),
		DefaultUpstreamModel:  d.Config.DefaultUpstreamModelName(),
		DefaultThinkingEffort: d.Config.DefaultThinkingEffort(),
	}
	if isEdit {
		// 局部重绘：异步任务同样要把 mask 合成进图片。
		//
		// Python 的 submit_edit 把 mask 塞进 payload，_run_task 再走
		// openai_v1_image_edit.handle → _composite_mask。Go 侧早期
		// taskCtx.Masks 从未被读取，异步链路里 mask 一样被静默丢弃。
		images, maskErr := protocol.CompositeMask(taskCtx.Images, taskCtx.Masks)
		if maskErr != nil {
			// 合成失败按任务失败处理（Python 里异常同样落到 _run_task 的
			// except Exception → status=error，error 文本进任务详情）。
			call.finish(maskErr, "", nil)
			return service.TaskResult{Error: maskErr.Error()}, nil
		}
		request.Images = images
	}

	pool := &protocol.ImagePool{
		Parallel: d.Config.ImageParallelGeneration(),
		Acquire: func(acquireCtx context.Context, index int) (*protocol.ImageGenerator, func(), error) {
			generator, release, err := d.acquireImageGenerator(acquireCtx, index)
			if err != nil {
				return nil, nil, err
			}
			// 把任务进度回调接进 generator。
			if taskCtx.Progress != nil {
				generator.OnProgress = taskCtx.Progress
			}
			return generator, release, nil
		},
	}

	result, err := pool.Run(ctx, request)
	if err != nil {
		call.finish(err, "", nil)
		return service.TaskResult{Error: err.Error()}, nil
	}

	collected := protocol.CollectImageOutputs(result.Outputs)
	delete(collected, "_account_email")

	data, _ := collected["data"].([]any)
	items := make([]map[string]any, 0, len(data))
	for _, item := range data {
		if entry, ok := item.(map[string]any); ok {
			items = append(items, entry)
		}
	}
	if len(items) == 0 {
		message := stringValueOf(collected["message"])
		if message == "" {
			message = "图片生成失败：上游未返回图片"
		}
		call.finish(errors.New(message), "", nil)
		return service.TaskResult{Error: message}, nil
	}

	// usage 在这里补算：CollectImageOutputs 只负责收 data/message，
	// 不算 token。同步接口在 handler 里算，异步任务没有 handler，
	// 必须在这里补上，否则任务详情里的 usage 永远是空的。
	usage := imageUsageFor(request, collected)
	conversationID := stringValueOf(collected["_conversation_id"])

	// 收集产出 URL 与账号邮箱，供日志页展示。
	urls := make([]string, 0, len(items))
	for _, item := range items {
		if url := stringValueOf(item["url"]); url != "" {
			urls = append(urls, url)
		}
	}
	call.finish(nil, stringValueOf(collected["_account_email"]), urls)

	return service.TaskResult{
		Data:           items,
		Usage:          usage,
		ConversationID: conversationID,
	}, nil
}
