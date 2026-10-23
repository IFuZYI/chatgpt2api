package protocol

import (
	"context"
	"strings"
	"time"
)

// Codex 图片链路（/backend-api/codex/responses）与 web 链路（/backend-api/f/conversation）
// 的结果形态完全不同，所以解析也必须分开：
//
//	web   → SSE 事件里只有 file_id / sediment_id，要再轮询对话文档 + 下载
//	codex → 事件里直接带 image_generation_call.result（base64），没有 file_id
//
// 因此 codex 不走 consumeStream（那套状态机是 web 的对话补丁协议），
// 而是走这里的独立解析。

// codexImageCollector 跨事件累积 codex 图片结果，并按 base64 内容去重。
//
// 去重必须**跨事件**：流式响应里同一个 image_generation_call 会同时出现在
// response.output_item.done 的 item 与 response.completed 的
// response.response.output 里。若每个事件各自去重（局部 seen），同一张图会被
// 收两次并返回两张一样的图 —— 早期实现就是这样，与注释声称的行为不符。
// Python 版正是如此（_codex_response_images 每次调用都新建列表），这里刻意偏离。
type codexImageCollector struct {
	images []string
	seen   map[string]bool
}

func newCodexImageCollector() *codexImageCollector {
	return &codexImageCollector{
		images: make([]string, 0, 2),
		seen:   map[string]bool{},
	}
}

// collect 递归遍历一个事件（或任意嵌套结构），收集 type=="image_generation_call"
// 的 result，归一成裸 base64 后追加（重复内容丢弃）。
//
// 复刻 Python 的 _codex_response_images（conversation.py:1245-1260）：
// result 可能是裸 base64，也可能带 "data:image/...;base64," 前缀
// （Python 在 :1250 用 split(",", 1)[1] 剥掉），两种都归一。
func (c *codexImageCollector) collect(value any) {
	var walk func(node any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			if stringOf(typed["type"]) == "image_generation_call" {
				if result, ok := typed["result"].(string); ok {
					trimmed := strings.TrimSpace(result)
					if strings.HasPrefix(trimmed, "data:image/") {
						if _, payload, found := strings.Cut(trimmed, ","); found {
							trimmed = strings.TrimSpace(payload)
						}
					}
					if trimmed != "" && !c.seen[trimmed] {
						c.seen[trimmed] = true
						c.images = append(c.images, trimmed)
					}
				}
			}
			for _, item := range typed {
				walk(item)
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(value)
}

// results 返回去重后的图片（裸 base64）。
func (c *codexImageCollector) results() []string { return c.images }

// generateCodexImage 跑一次 codex 图片生成。
//
// 与 web 路径的三处刻意差异（对齐 Python stream_codex_image_outputs）：
//  1. prompt 不做 build_image_prompt / global_system_prompt 拼接 —— 尺寸与质量
//     走 tools[0] 的 size/quality，不走提示词。
//  2. 没有 conversation_id，因此不轮询、不下载、不清理对话。
//  3. 图片结果直接内联在事件里，拿到即结果。
func (g *ImageGenerator) generateCodexImage(ctx context.Context, request ImageRequest) ([]ImageOutput, error) {
	index, total := 1, 1
	outputs := make([]ImageOutput, 0, 4)

	payloads, errs, streamErr := g.Upstream.StreamImage(ctx, ImageStreamRequest{
		Prompt:  request.Prompt,
		Model:   request.Model,
		Images:  request.Images,
		Size:    request.Size,
		Quality: request.Quality,
		Progress: func(step string) {
			if g.OnProgress != nil {
				g.OnProgress(step)
			}
		},
	})
	if streamErr != nil {
		return outputs, wrapImageError(streamErr, request.Model, "")
	}

	// collector 存活于整个流，所以同一个 item 出现在多个事件里也只收一次。
	collector := newCodexImageCollector()
	for payload := range payloads {
		if payload == "[DONE]" {
			break
		}
		var event map[string]any
		if !decodeObject(payload, &event) {
			continue
		}
		collector.collect(event)
	}
	for streamErr := range errs {
		if streamErr != nil {
			return outputs, wrapImageError(streamErr, request.Model, "")
		}
	}

	images := collector.results()
	if len(images) == 0 {
		// Python: raise ImageGenerationError("No image result found in response")
		return outputs, &ImageGenerationError{
			Message:    "No image result found in response",
			StatusCode: 502,
			ErrorType:  "server_error",
			Code:       "upstream_error",
		}
	}

	items := make([]map[string]any, 0, len(images))
	for _, image := range images {
		items = append(items, map[string]any{
			"b64_json":       image,
			"revised_prompt": request.Prompt,
		})
	}

	result, saveErr := FormatImageResult(items, request.Prompt, request.ResponseFormat,
		g.SaveImage, request.BaseURL, time.Now().Unix(), "")
	if saveErr != nil {
		// 落盘失败要冒泡成生成失败（与 web 链路一致）。
		return outputs, wrapImageError(saveErr, request.Model, "")
	}
	data, _ := result["data"].([]any)
	if len(data) == 0 {
		return outputs, &ImageGenerationError{
			Message:    "No image result found in response",
			StatusCode: 502,
			ErrorType:  "server_error",
			Code:       "upstream_error",
		}
	}

	converted := make([]map[string]any, 0, len(data))
	for _, item := range data {
		if entry, ok := item.(map[string]any); ok {
			converted = append(converted, entry)
		}
	}
	outputs = append(outputs, ImageOutput{
		Kind: ImageKindResult, Model: request.Model, Index: index, Total: total,
		Data: converted,
	})
	if g.OnResult != nil {
		g.OnResult()
	}
	return outputs, nil
}
