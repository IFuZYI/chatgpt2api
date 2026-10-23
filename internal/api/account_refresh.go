package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

// registerAccountRefresh 注册刷新与重新登录（都是异步 + 进度轮询）。
func (d *Deps) registerAccountRefresh(r *gin.Engine) {
	r.POST("/api/accounts/refresh", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		tokens := stringList(body["access_tokens"])
		if len(tokens) == 0 {
			tokens = d.Accts.ListTokens()
		}
		if len(tokens) == 0 {
			writeErr(c, Errorf(http.StatusBadRequest, "access_tokens is required"))
			return
		}

		progressID := newUUIDString()
		// 异步跑：前端拿 progress_id 轮询。这里必须脱离请求 ctx，
		// 否则客户端断开就中断刷新（与 Python 的 asyncio.create_task 语义对齐）。
		//
		// 但也不能裸跑：刷新会写 accounts.json，进程退出时必须能取消它，
		// 否则 JSON 后端可能正写到一半。登记到 Tasks.StartBackground，
		// 关闭时先取消再等待（30 分钟的 ctx 不可能硬等）。
		refreshCtx, finishRefresh, ok := d.Tasks.StartBackground(context.Background(), 30*time.Minute)
		if !ok {
			writeErr(c, Errorf(http.StatusServiceUnavailable, "server is shutting down"))
			return
		}
		go func() {
			defer finishRefresh()
			d.Accts.RefreshAccounts(refreshCtx, tokens, progressID, d.refreshVerifier())
		}()
		c.JSON(http.StatusOK, gin.H{"progress_id": progressID})
	})

	r.GET("/api/accounts/refresh/progress/:progress_id", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		progress := d.Accts.GetRefreshProgress(c.Param("progress_id"))
		if progress == nil {
			writeErr(c, Errorf(http.StatusNotFound, "progress not found"))
			return
		}
		c.JSON(http.StatusOK, progress)
	})

	r.POST("/api/accounts/re-login", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		tokens := stringList(body["access_tokens"])
		if len(tokens) == 0 {
			writeErr(c, Errorf(http.StatusBadRequest, "access_tokens is required"))
			return
		}
		progressID := newUUIDString()
		go d.runRelogin(tokens, progressID)
		c.JSON(http.StatusOK, gin.H{"progress_id": progressID})
	})

	r.GET("/api/accounts/re-login/progress/:progress_id", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		progress := d.Accts.GetReloginProgress(c.Param("progress_id"))
		if progress == nil {
			writeErr(c, Errorf(http.StatusNotFound, "progress not found"))
			return
		}
		c.JSON(http.StatusOK, progress)
	})
}

// refreshVerifier 把上游的账号信息查询包装成 service 需要的回调。
func (d *Deps) refreshVerifier() service.RefreshVerifier {
	return func(ctx context.Context, token, event string) (service.Account, error) {
		if d.AccountInfoFetcher == nil {
			return nil, errNotConfigured
		}
		return d.AccountInfoFetcher(ctx, token)
	}
}

// runRelogin 是密码重登的占位实现。
//
// 完整的密码登录流程（sentinel → authorize → 验证码 → token 交换）
// 依赖 OAuth 服务，尚未移植。这里如实标记为未实现，
// 而不是假装成功——否则用户会看到「重登完成」但账号仍然是坏的。
func (d *Deps) runRelogin(tokens []string, progressID string) {
	d.Accts.InitReloginProgress(progressID, len(tokens))
	for _, token := range tokens {
		d.Accts.UpdateReloginProgress(progressID, token, "error", "password re-login is not implemented yet")
	}
	d.Accts.FinishReloginProgress(progressID, map[string]any{
		"relogined": 0,
		"errors":    []any{map[string]any{"error": "password re-login is not implemented yet"}},
	}, "password re-login is not implemented yet")
}

// registerAccountExport 注册导出。
func (d *Deps) registerAccountExport(r *gin.Engine) {
	r.POST("/api/accounts/export", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		format := strings.TrimSpace(stringValueOf(body["format"]))
		if format == "" {
			format = "json"
		}
		items := d.Accts.BuildExportItems(stringList(body["access_tokens"]))
		if len(items) == 0 {
			writeErr(c, Errorf(http.StatusBadRequest,
				"没有可导出的完整账号，需要同时有 access_token、refresh_token 和 id_token"))
			return
		}

		timestamp := time.Now().Format("20060102-150405")
		if format == "zip" {
			payload, zipErr := buildAccountZip(items)
			if zipErr != nil {
				writeErr(c, Errorf(http.StatusInternalServerError, zipErr.Error()))
				return
			}
			c.Header("Content-Disposition", `attachment; filename="codex-accounts-`+timestamp+`.zip"`)
			c.Data(http.StatusOK, "application/zip", payload)
			return
		}

		// 单个账号导出成对象，多个导出成数组（与 Python 一致）。
		var payload any = items
		if len(items) == 1 {
			payload = items[0]
		}
		encoded, err := marshalJSON(payload)
		if err != nil {
			writeErr(c, Errorf(http.StatusInternalServerError, err.Error()))
			return
		}
		c.Header("Content-Disposition", `attachment; filename="codex-accounts-`+timestamp+`.json"`)
		c.Data(http.StatusOK, "application/json", []byte(encoded+"\n"))
	})
}
