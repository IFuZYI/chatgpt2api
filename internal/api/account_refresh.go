package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/service"
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
			// 整池刷新必须排除禁用/需人工的账号。
			//
			// 用 ListTokens 会把禁用号也拉进来：刷新成功时
			// applyRefreshedAccount 合并上游 status，禁用会被复活成「正常」。
			// 这是面板上「选一个禁用行点刷新就变绿」的直接成因。
			tokens = d.Accts.ListMaintainableTokens()
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
		goSafe(d.Logs, "刷新账号", func() {
			defer finishRefresh()
			d.Accts.RefreshAccounts(refreshCtx, tokens, progressID, d.refreshVerifier())
		})
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
		// 去重 + 同步建进度：必须发生在返回 progress_id 之前，
		// 否则前端第一次轮询会 404（它会在 300ms 后就开始轮询）。
		// 顺序上先确认后台任务能被托管，避免关机时留下永远 running 的进度。
		reloginCtx, finishRelogin, ok := d.Tasks.StartBackground(context.Background(), 30*time.Minute)
		if !ok {
			writeErr(c, Errorf(http.StatusServiceUnavailable, "server is shutting down"))
			return
		}
		tokens = service.DedupeReloginTokens(tokens)
		progressID := newUUIDString()
		d.Accts.InitReloginProgress(progressID, len(tokens))
		goSafe(d.Logs, "重登账号", func() {
			defer finishRelogin()
			d.runRelogin(reloginCtx, tokens, progressID)
		})
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

// runRelogin 执行密码重新登录（含 TOTP 两步验证）。
//
// 与 /api/accounts/refresh 一样是异步 + 进度轮询：后台 ctx 由 Tasks 托管，
// 进程退出时会先取消再等待（重登会写 accounts.json，不能裸跑 goroutine）。
func (d *Deps) runRelogin(ctx context.Context, tokens []string, progressID string) {
	d.Accts.ReloginAccounts(ctx, tokens, progressID, d.PasswordLoginFunc())
}

// passwordLoginFunc 把上游客户端工厂 + LoginWithPassword 组合成 service 需要的执行体。
//
// 每个账号一个新会话：登录会往 cookie jar 里塞 oai-did / oai-sc，
// 复用会话会让不同账号的 challenge 互相污染。
func (d *Deps) PasswordLoginFunc() service.PasswordLoginFunc {
	if d.PasswordLoginClient == nil {
		return nil
	}
	return func(ctx context.Context, proxyURL, email, password, totpSecret string) (map[string]string, error) {
		return service.LoginWithPasswordVia(ctx, d.PasswordLoginClient, proxyURL, email, password, totpSecret)
	}
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
