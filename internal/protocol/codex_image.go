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

// codexResponseImages 复刻 Python 的 _codex_response_images：
// 递归遍历事件里的任意结构，收集 type=="image_generation_call" 的 result。
//
// 结果可能是裸 base64，也可能带 "data:image/...;base64," 前缀（Python 在
// :1250 用 split(",", 1)[1] 剥掉前缀），两种都归一成裸 base64。
//
// 与 Python 的差异：这里对完全相同的 base64 去重。流式响应里同一个 item 可能
// 同时出现在 response.output_item.done 与 response.completed 的 response.output
// 里，Python 会把同一张图收两次并返回重复图片；去重后的行为才是用户期望的。
func codexResponseImages(value any) []string {
	out := make([]string, 0, 2)
	seen := map[string]bool{}

	var walk func(node any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			if stringOf(typed["type"]) == "image_generation_call" {
				if result, ok := typed["result"].(string); ok {
					trimmed := strings.TrimSpace(result)
					if trimmed != "" {
						if strings.HasPrefix(trimmed, "data:image/") {
							if _, payload, found := strings.Cut(trimmed, ","); found {
								trimmed = payload
							}
						}
						if !seen[trimmed] {
							seen[trimmed] = true
							out = append(out, trimmed)
						}
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
	return out
}

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

	images := make([]string, 0, 2)
	for payload := range payloads {
		if payload == "[DONE]" {
			break
		}
		var event map[string]any
		if !decodeObject(payload, &event) {
			continue
		}
		for _, image := range codexResponseImages(event) {
			images = append(images, image)
		}
	}
	for streamErr := range errs {
		if streamErr != nil {
			return outputs, wrapImageError(streamErr, request.Model, "")
		}
	}

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

	result := FormatImageResult(items, request.Prompt, request.ResponseFormat,
		g.SaveImage, request.BaseURL, time.Now().Unix(), "")
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
