package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/gin-gonic/gin"
)

// registerRemainingRoutes 挂载剩余端点：/v1/responses、/v1/search、
// /v1/editable-file-tasks、/files/*、/v1/ppt|psd/generations。
func (d *Deps) registerRemainingRoutes(r *gin.Engine) {
	// ---- /v1/responses ----
	r.POST("/v1/responses", func(c *gin.Context) {
		if _, err := d.RequireIdentityCtx(c); err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		// Responses 的图片工具调用在 M4 之后接入；当前只支持纯文本。
		if hasImageGenerationTool(body) {
			writeErr(c, Errorf(http.StatusNotImplemented, "image generation tool is not implemented yet"))
			return
		}
		d.handleTextResponses(c, body)
	})

	// ---- /v1/search ----
	r.POST("/v1/search", func(c *gin.Context) {
		identity, err := d.RequireIdentityCtx(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		call := d.beginCall("网页搜索", c.FullPath(),
			strings.TrimSpace(stringValueOf(body["model"])), identity.Name)
		defer func() {
			call.finish(errors.New("web search is not implemented yet"), "", nil)
		}()
		if strings.TrimSpace(stringValueOf(body["prompt"])) == "" {
			writeErr(c, validationError("prompt", "Field required", nil))
			return
		}
		writeErr(c, Errorf(http.StatusNotImplemented, "web search is not implemented yet"))
	})

	// ---- 可编辑文件任务 ----
	r.GET("/v1/editable-file-tasks", func(c *gin.Context) {
		if _, err := d.RequireIdentity(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": []any{}, "missing_ids": parseTaskIDs(c.Query("ids"))})
	})

	r.POST("/v1/ppt/generations", func(c *gin.Context) {
		if _, err := d.RequireIdentityCtx(c); err != nil {
			writeErr(c, err)
			return
		}
		call := d.beginCall("PPT 生成", c.FullPath(), "", c.GetString("key_name"))
		defer func() {
			call.finish(errors.New("editable PPT generation is not implemented yet"), "", nil)
		}()
		writeErr(c, Errorf(http.StatusNotImplemented, "editable PPT generation is not implemented yet"))
	})

	r.POST("/v1/psd/generations", func(c *gin.Context) {
		if _, err := d.RequireIdentityCtx(c); err != nil {
			writeErr(c, err)
			return
		}
		call := d.beginCall("PSD 生成", c.FullPath(), "", c.GetString("key_name"))
		defer func() {
			call.finish(errors.New("editable PSD generation is not implemented yet"), "", nil)
		}()
		writeErr(c, Errorf(http.StatusNotImplemented, "editable PSD generation is not implemented yet"))
	})

	// ---- 文件下载 ----
	r.GET("/files/*file_path", func(c *gin.Context) {
		rel := strings.TrimPrefix(c.Param("file_path"), "/")
		path, pathErr := d.resolveEditableFile(rel)
		if pathErr != nil {
			writeErr(c, StatusError(http.StatusNotFound, "file not found"))
			return
		}
		c.File(path)
	})
}

// hasImageGenerationTool 检查 tools 里是否有 image_generation。
func hasImageGenerationTool(body map[string]any) bool {
	tools, ok := body["tools"].([]any)
	if !ok {
		return false
	}
	for _, tool := range tools {
		entry, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(stringValueOf(entry["type"])) == "image_generation" {
			return true
		}
	}
	return false
}

// handleTextResponses 走 Responses 协议的文本链路。
//
// 输出结构与 chat completions 不同：response 对象 + output 数组。
func (d *Deps) handleTextResponses(c *gin.Context, body map[string]any) {
	model := orDefaultValue(strings.TrimSpace(stringValueOf(body["model"])), "auto")
	messages := protocol.MessagesFromInput(body["input"], body["instructions"])
	if len(messages) == 0 {
		writeErr(c, Errorf(http.StatusBadRequest, "input is required"))
		return
	}

	settings := d.chatCacheSettings()
	normalized := protocol.NormalizeTextMessages(messages, settings)
	stream := truthyValue(body["stream"])

	var text strings.Builder
	produce := func(ctx context.Context, onDelta func(string)) error {
		return d.runTextConversation(ctx, model, normalized, protocol.ThinkingEffortFromBody(body), onDelta)
	}

	if err := produce(c.Request.Context(), func(delta string) { text.WriteString(delta) }); err != nil {
		writeErr(c, mapProtocolError(err))
		return
	}

	content := text.String()
	response := protocol.ResponseObject(model, content, normalized)
	if stream {
		// 流式 Responses 用事件序列；这里复用协议层的构造。
		events := protocol.ResponseStreamEvents(response)
		writeSSEChunks(c, events)
		return
	}
	c.JSON(http.StatusOK, response)
}
