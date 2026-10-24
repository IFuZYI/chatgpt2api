package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/gin-gonic/gin"
)

// EditableUpstreamFactory 为一次可编辑文件导出构造上游客户端。
// 由 main 注入（与 TextUpstream/ImageUpstream 同理：api 不依赖 upstream）。
type EditableUpstreamFactory func(ctx context.Context, accessToken string) (protocol.EditableFileUpstream, func(), error)

// registerEditableFileRoutes 挂载可编辑文件（PPT/PSD）任务与产物下载。
//
// 复刻 api/ai.py:178-211：
//   - GET  /v1/editable-file-tasks  按 owner 列任务（ids 逗号分隔）
//   - POST /v1/ppt/generations      建 PPT 任务（异步）
//   - POST /v1/psd/generations      建 PSD 任务（异步）
//   - GET  /files/*                 下载产物（无需鉴权，与 Python 一致）
func (d *Deps) registerEditableFileRoutes(r *gin.Engine) {
	r.GET("/v1/editable-file-tasks", func(c *gin.Context) {
		identity, authErr := d.RequireIdentity(c.GetHeader("Authorization"))
		if authErr != nil {
			writeErr(c, authErr)
			return
		}
		if d.EditableTasks == nil {
			writeErr(c, Errorf(http.StatusServiceUnavailable, "editable file tasks are not configured"))
			return
		}
		c.JSON(http.StatusOK, d.EditableTasks.ListTasks(identity.ID, parseTaskIDs(c.Query("ids"))))
	})

	r.POST("/v1/ppt/generations", func(c *gin.Context) {
		d.handleEditableFileSubmit(c, protocol.EditableKindPPT)
	})
	r.POST("/v1/psd/generations", func(c *gin.Context) {
		d.handleEditableFileSubmit(c, protocol.EditableKindPSD)
	})

	// 产物下载：路径穿越与不存在的文件都返回 404 {"detail":"file not found"}，
	// 与 Python 的 `raise HTTPException(404, {"error": "file not found"})` 信封一致
	// （管理面信封下 detail 是对象，见 errors.go 的 EnvelopeAdmin）。
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

// handleEditableFileSubmit 处理 PPT/PSD 两个提交端点。
func (d *Deps) handleEditableFileSubmit(c *gin.Context, kind string) {
	identity, authErr := d.RequireIdentityCtx(c)
	if authErr != nil {
		writeErr(c, authErr)
		return
	}
	if d.EditableTasks == nil {
		writeErr(c, Errorf(http.StatusServiceUnavailable, "editable file tasks are not configured"))
		return
	}
	body, bodyErr := decodeJSONBody(c)
	if bodyErr != nil {
		writeErr(c, bodyErr)
		return
	}

	prompt := stringValueOf(body["prompt"])
	clientTaskID := strings.TrimSpace(stringValueOf(body["client_task_id"]))
	base64Images := stringList(body["base64_images"])

	summary := editableTaskSummary(kind)
	call := d.beginCall(summary, c.FullPath(), protocol.EditableFileModel, identity.Name)

	// Python 用 filter_or_log 在路由层查内容，失败会记一条失败日志。
	if filterErr := d.checkContent(c.Request.Context(), prompt); filterErr != nil {
		call.finish(filterErr, "", nil)
		writeErr(c, contentFilterError(filterErr))
		return
	}

	task, submitErr := d.EditableTasks.Submit(
		identity.ID, clientTaskID, identity.Name, kind, prompt, base64Images,
		resolveImageBaseURL(c, d),
	)
	if submitErr != nil {
		call.finish(submitErr, "", nil)
		writeErr(c, Errorf(http.StatusBadRequest, submitErr.Error()))
		return
	}
	// 任务已入队：完成/失败的调用日志由任务执行体写（与 Python 的
	// editable_file_task_service._log_call 位置一致），这里不写成功日志。
	c.JSON(http.StatusOK, task)
}

// editableTaskSummary 复刻 _log_call 里的 summary（PPT生成任务 / PSD生成任务）。
func editableTaskSummary(kind string) string {
	if kind == protocol.EditableKindPSD {
		return "PSD生成任务"
	}
	return "PPT生成任务"
}
