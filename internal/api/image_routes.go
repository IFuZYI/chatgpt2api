package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

// RegisterImageRoutes 注册图片相关的读接口与文件服务。
// 写接口（生成/编辑/任务）在 M4 补齐。
func (d *Deps) RegisterImageRoutes(r *gin.Engine) {
	// ---- 图片列表与管理 ----
	r.GET("/api/images", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, d.Images.ListResponse(
			resolveImageBaseURL(c, d),
			strings.TrimSpace(c.Query("start_date")),
			strings.TrimSpace(c.Query("end_date")),
		))
	})

	r.POST("/api/images/delete", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, d.Images.DeleteImages(
			stringList(body["paths"]),
			strings.TrimSpace(stringValueOf(body["start_date"])),
			strings.TrimSpace(stringValueOf(body["end_date"])),
			truthyValue(body["all_matching"]),
		))
	})

	r.POST("/api/images/download", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		payload, zipErr := d.Images.DownloadZip(stringList(body["paths"]))
		if zipErr != nil {
			writeErr(c, Errorf(http.StatusNotFound, "no images found"))
			return
		}
		c.Header("Content-Disposition", `attachment; filename="images.zip"`)
		c.Data(http.StatusOK, "application/zip", payload)
	})

	r.GET("/api/images/download/*image_path", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		rel := strings.TrimPrefix(c.Param("image_path"), "/")
		payload, mime, err := d.Images.ReadImage(rel)
		if err != nil {
			writeErr(c, StatusError(http.StatusNotFound, "image not found"))
			return
		}
		c.Header("Content-Disposition", `attachment; filename="`+pathBase(rel)+`"`)
		c.Data(http.StatusOK, mime, payload)
	})

	r.GET("/api/images/storage", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, d.Images.StorageStats())
	})

	r.POST("/api/images/storage/compress", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, d.Images.CompressImages())
	})

	r.POST("/api/images/storage/cleanup-to-target", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		target := 500
		if raw := strings.TrimSpace(c.Query("target_free_mb")); raw != "" {
			if parsed, convErr := strconv.Atoi(raw); convErr == nil {
				target = parsed
			}
		}
		// dry_run 必须接受 1/true/yes/on（FastAPI 的 bool 解析语义）。
		//
		// 只认 "true" 会让 ?dry_run=1 变成**真删**：前端与脚本习惯用 1，
		// 用户以为在演练，实际图片被删光（已实测复现）。
		dryRun := parseBoolQuery(c.Query("dry_run"))
		c.JSON(http.StatusOK, d.Images.DeleteToTarget(target, dryRun))
	})

	// ---- 标签 ----
	r.GET("/api/images/tags", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"tags": d.Images.AllTags()})
	})

	r.POST("/api/images/tags", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		rel := strings.TrimLeft(strings.TrimSpace(stringValueOf(body["path"])), "/")
		if rel == "" {
			writeErr(c, Errorf(http.StatusBadRequest, "path is required"))
			return
		}
		tags := d.Images.SetTags(rel, stringList(body["tags"]))
		c.JSON(http.StatusOK, gin.H{"ok": true, "tags": tags})
	})

	r.DELETE("/api/images/tags/:tag", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		removed := d.Images.DeleteTag(c.Param("tag"))
		c.JSON(http.StatusOK, gin.H{"ok": true, "removed_from": removed})
	})

	// ---- 图片文件服务（无需鉴权，与 Python 一致）----
	imageFileHeaders := func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "*")
	}

	r.GET("/images/*image_path", func(c *gin.Context) {
		imageFileHeaders(c)
		rel := strings.TrimPrefix(c.Param("image_path"), "/")
		payload, mime, err := d.Images.ReadImage(rel)
		if err != nil {
			writeErr(c, StatusError(http.StatusNotFound, "image not found"))
			return
		}
		c.Data(http.StatusOK, mime, payload)
	})

	r.GET("/image-thumbnails/*image_path", func(c *gin.Context) {
		imageFileHeaders(c)
		rel := strings.TrimPrefix(c.Param("image_path"), "/")
		path, err := d.Images.EnsureThumbnail(rel)
		if err != nil {
			writeErr(c, StatusError(http.StatusNotFound, "image not found"))
			return
		}
		c.File(path)
	})

	// ---- 图片任务 ----
	r.GET("/api/image-tasks", func(c *gin.Context) {
		identity, err := d.RequireIdentity(c.GetHeader("Authorization"))
		if err != nil {
			writeErr(c, err)
			return
		}
		ids := parseTaskIDs(c.Query("ids"))
		c.JSON(http.StatusOK, d.Tasks.ListTasks(identity.ID, ids))
	})

	r.POST("/api/image-tasks/generations", func(c *gin.Context) {
		identity, err := d.RequireIdentity(c.GetHeader("Authorization"))
		if err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		clientTaskID := strings.TrimSpace(stringValueOf(body["client_task_id"]))
		if clientTaskID == "" {
			writeErr(c, validationError("client_task_id", "Field required", nil))
			return
		}
		prompt := strings.TrimSpace(stringValueOf(body["prompt"]))
		if prompt == "" {
			writeErr(c, validationError("prompt", "Field required", nil))
			return
		}
		task, submitErr := d.Tasks.Submit(
			identity.ID, clientTaskID, "generate", prompt,
			orDefaultValue(stringValueOf(body["model"]), "gpt-image-2"),
			stringValueOf(body["size"]),
			orDefaultValue(stringValueOf(body["quality"]), "auto"),
			resolveImageBaseURL(c, d),
			nil, nil,
		)
		if submitErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, submitErr.Error()))
			return
		}
		c.JSON(http.StatusOK, task)
	})

	r.POST("/api/image-tasks/edits", func(c *gin.Context) {
		identity, err := d.RequireIdentity(c.GetHeader("Authorization"))
		if err != nil {
			writeErr(c, err)
			return
		}
		payload, images, masks, parseErr := parseImageEditRequest(c)
		if parseErr != nil {
			writeErr(c, parseErr)
			return
		}
		clientTaskID := strings.TrimSpace(stringValueOf(payload["client_task_id"]))
		if clientTaskID == "" {
			writeErr(c, Errorf(http.StatusBadRequest, "client_task_id is required"))
			return
		}
		task, submitErr := d.Tasks.Submit(
			identity.ID, clientTaskID, "edit",
			strings.TrimSpace(stringValueOf(payload["prompt"])),
			orDefaultValue(stringValueOf(payload["model"]), "gpt-image-2"),
			stringValueOf(payload["size"]),
			orDefaultValue(stringValueOf(payload["quality"]), "auto"),
			resolveImageBaseURL(c, d),
			images, masks,
		)
		if submitErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, submitErr.Error()))
			return
		}
		c.JSON(http.StatusOK, task)
	})

	r.POST("/api/image-tasks/:task_id/resume-poll", func(c *gin.Context) {
		identity, err := d.RequireIdentity(c.GetHeader("Authorization"))
		if err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		extraTimeout := 30.0
		if value, ok := body["extra_timeout_secs"]; ok {
			parsed := floatOf(value)
			if parsed < 5 || parsed > 120 {
				writeErr(c, validationError("extra_timeout_secs",
					"Input should be greater than or equal to 5", map[string]any{"ge": 5.0}))
				return
			}
			extraTimeout = parsed
		}
		task := d.Tasks.GetTask(identity.ID, c.Param("task_id"))
		if task == nil {
			writeErr(c, Errorf(http.StatusBadRequest, "task not found"))
			return
		}
		if stringValueOf(task["status"]) != service.TaskStatusError {
			writeErr(c, Errorf(http.StatusBadRequest, "task is not in error state"))
			return
		}
		if !strings.Contains(stringValueOf(task["error"]), "超时") {
			writeErr(c, Errorf(http.StatusBadRequest, "task error is not a timeout error"))
			return
		}
		if stringValueOf(task["conversation_id"]) == "" {
			writeErr(c, Errorf(http.StatusBadRequest, "task has no conversation_id"))
			return
		}
		updated := d.Tasks.UpdateTask(identity.ID, c.Param("task_id"), map[string]any{
			"status": service.TaskStatusRunning,
			"error":  "",
		})
		// resume-poll 也会写 image_tasks.json，必须纳入关闭等待：
		// 否则进程退出时它可能正写到一半（服务关闭语义见 ImageTaskService.Close）。
		//
		// 参数必须在起 goroutine 之前取出来：gin.Context 在 handler 返回后会被
		// 复用，goroutine 里再读 c.Param 是数据竞争。
		ownerID := identity.ID
		taskID := c.Param("task_id")
		conversationID := stringValueOf(task["conversation_id"])
		if release, ok := d.Tasks.TrackBackground(); ok {
			go func() {
				defer release()
				d.runResumePoll(ownerID, taskID, conversationID, extraTimeout)
			}()
		}
		c.JSON(http.StatusOK, updated)
	})
}

// validationError 构造 Pydantic 风格的 422 条目（缺字段）。
func validationError(field, message string, ctx map[string]any) *HTTPError {
	entry := map[string]any{
		"type":  "missing",
		"loc":   []any{"body", field},
		"msg":   message,
		"input": nil,
	}
	if ctx != nil {
		entry["type"] = "greater_than_equal"
		entry["ctx"] = ctx
	}
	return ErrorDetail(http.StatusUnprocessableEntity, []any{entry})
}

// rangeValidationError 构造 Pydantic 的越界条目（ge/le）。
//
// Pydantic 对 `n: int = Field(ge=1, le=4)` 的越界报两种 type：
//   - n < 1  → greater_than_equal，ctx {"ge": 1}
//   - n > 4  → less_than_equal，ctx {"le": 4}
//
// input 要带上原值，客户端靠它定位是哪个参数越界。
func rangeValidationError(field string, value int, lower, upper int) *HTTPError {
	entry := map[string]any{
		"loc":   []any{"body", field},
		"input": value,
	}
	switch {
	case value < lower:
		entry["type"] = "greater_than_equal"
		entry["msg"] = fmt.Sprintf("Input should be greater than or equal to %d", lower)
		entry["ctx"] = map[string]any{"ge": lower}
	default:
		entry["type"] = "less_than_equal"
		entry["msg"] = fmt.Sprintf("Input should be less than or equal to %d", upper)
		entry["ctx"] = map[string]any{"le": upper}
	}
	return ErrorDetail(http.StatusUnprocessableEntity, []any{entry})
}

// runResumePoll 复刻 _run_resume_poll：继续轮询已有 conversation_id 的任务。
func (d *Deps) runResumePoll(ownerID, taskID, conversationID string, extraTimeout float64) {
	started := time.Now()
	if d.ImageUpstream == nil {
		d.Tasks.UpdateTask(ownerID, taskID, map[string]any{
			"status": service.TaskStatusError,
			"error":  "image upstream is not configured",
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(extraTimeout+60)*time.Second)
	defer cancel()

	// 用空 token 构造上游：任务记录里不存 access_token（只有 conversation_id）。
	//
	// 这是已知缺口：上游对空 Authorization 返回 401，所以「继续等待」
	// 在没有 token 的情况下必然失败。要真正修好，需要在 Submit 时把
	// token 落进任务记录（或让 resume-poll 重新取号 + 复用 conversation_id）。
	// 两条路都会改变 data/image_tasks.json 的字段集，属独立决策，
	// 本轮不擅自扩字段 —— 失败时明确报错而不是静默超时。
	accessToken := ""
	upstream, closeFn, err := d.ImageUpstream(ctx, accessToken)
	if err != nil {
		d.Tasks.UpdateTask(ownerID, taskID, map[string]any{
			"status": service.TaskStatusError,
			"error":  err.Error(),
		})
		return
	}
	defer closeFn()

	options := d.pollOptions(accessToken)
	options.TimeoutSecs = extraTimeout
	result, pollErr := protocol.PollImageResults(ctx, upstream, conversationID, options, nil, nil)
	if pollErr != nil || (len(result.FileIDs) == 0 && len(result.SedimentIDs) == 0) {
		message := "resume poll failed"
		if pollErr != nil {
			message = pollErr.Error()
		} else {
			message = "继续等待 " + itoaFloat(extraTimeout) + " 秒后仍未找到图片结果。"
		}
		d.Tasks.UpdateTask(ownerID, taskID, map[string]any{
			"status":      service.TaskStatusError,
			"error":       message,
			"data":        []any{},
			"duration_ms": time.Since(started).Milliseconds(),
		})
		return
	}

	urls, resolveErr := upstream.ResolveImageURLs(ctx, conversationID, result.FileIDs, result.SedimentIDs)
	if resolveErr != nil || len(urls) == 0 {
		d.Tasks.UpdateTask(ownerID, taskID, map[string]any{
			"status": service.TaskStatusError,
			"error":  "图片 URL 解析失败",
			"data":   []any{},
		})
		return
	}

	items := make([]map[string]any, 0, len(urls))
	for _, url := range urls {
		payload, downloadErr := upstream.DownloadImage(ctx, url)
		if downloadErr != nil || len(payload) == 0 {
			continue
		}
		items = append(items, map[string]any{
			"b64_json": base64.StdEncoding.EncodeToString(payload),
		})
	}

	formatted, saveErr := protocol.FormatImageResult(items, "", "b64_json",
		func(payload []byte, baseURL string) (string, error) {
			_, publicURL, saveErr := d.Images.SaveImageBytes(payload, baseURL)
			return publicURL, saveErr
		}, "", time.Now().Unix(), "")
	if saveErr != nil {
		// 落盘失败（WebDAV 5xx / 磁盘满）要让任务失败，不能报 success 却给空 url。
		d.Tasks.UpdateTask(ownerID, taskID, map[string]any{
			"status": service.TaskStatusError,
			"error":  saveErr.Error(),
			"data":   []any{},
		})
		return
	}
	data, _ := formatted["data"].([]any)

	d.Tasks.UpdateTask(ownerID, taskID, map[string]any{
		"status":      service.TaskStatusSuccess,
		"data":        data,
		"error":       "",
		"duration_ms": time.Since(started).Milliseconds(),
	})
}

// floatOf 容错取浮点数。
func floatOf(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	default:
		return 0
	}
}

// itoaFloat 把浮点秒数转成整数文案（Python 用 int 格式化）。
func itoaFloat(value float64) string {
	return itoa(int(value))
}

func parseTaskIDs(raw string) []string {
	out := make([]string, 0)
	for _, item := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func pathBase(path string) string {
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		return path[index+1:]
	}
	return path
}

// resolveImageBaseURL 复刻 Python 的 resolve_image_base_url：
// config.base_url 优先，否则用请求的 scheme://host。
func resolveImageBaseURL(c *gin.Context, d *Deps) string {
	if base := d.Config.BaseURL(); base != "" {
		return base
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")); forwarded != "" {
		scheme = forwarded
	}
	host := c.GetHeader("Host")
	if host == "" {
		host = c.Request.Host
	}
	return scheme + "://" + host
}

func truthyValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		lowered := strings.ToLower(strings.TrimSpace(typed))
		return lowered == "1" || lowered == "true" || lowered == "yes" || lowered == "on"
	case float64:
		return typed != 0
	default:
		return false
	}
}

// parseBoolQuery 按 FastAPI/Pydantic 的宽松规则解析布尔查询参数。
func parseBoolQuery(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on", "t", "y":
		return true
	default:
		return false
	}
}
