package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

// RegisterSettingsRoutes 注册 /api/settings 系列。
// M6 会在这里补齐备份、WebDAV、图片管理等分支。
func (d *Deps) RegisterSettingsRoutes(r *gin.Engine) {
	r.GET("/api/settings", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		// 脱敏：配置里含 R2 密钥、备份口令、WebDAV 密码、AI 审核 key。
		// 前端靠 has_* 标记显示「已设置」，回写时保留旧值。
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"config": maskConfigSecrets(d.Config.Get())})
	})

	r.POST("/api/settings", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		updated, updateErr := d.Config.Update(body)
		if updateErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, updateErr.Error()))
			return
		}
		// 与 GET 保持一致：Update 返回的是 s.Get()，其中含
		// webdav_password / R2 密钥 / AI key 的**原文**。
		// 不脱敏就等于每次保存设置都把密钥明文送回浏览器。
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"config": maskConfigSecrets(updated)})
	})

	r.GET("/api/proxy/runtime", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"runtime": d.Config.PublicProxyRuntimeSettings(),
			"status":  d.proxyRuntimeStatus(),
		})
	})

	r.POST("/api/proxy/runtime", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		if _, updateErr := d.Config.Update(map[string]any{"proxy_runtime": body}); updateErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, updateErr.Error()))
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"runtime": d.Config.PublicProxyRuntimeSettings(),
			"status":  d.proxyRuntimeStatus(),
		})
	})

	r.POST("/api/proxy/test", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		result := service.TestProxy(d.Proxy, stringValueOf(body["url"]), 15*time.Second)
		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	r.POST("/api/proxy/clearance/test", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		target := stringValueOf(body["target_url"])
		if strings.TrimSpace(target) == "" {
			target = "https://chatgpt.com"
		}
		c.JSON(http.StatusOK, gin.H{"result": service.TestClearance(d.Proxy, target)})
	})

	// ---- 存储信息 ----
	r.GET("/api/storage/info", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"backend": d.Storage.BackendInfo(),
			"health":  d.Storage.HealthCheck(),
		})
	})

	// ---- 备份 ----
	d.registerBackupRoutes(r)

	// ---- 图片存储（WebDAV）----
	r.POST("/api/image-storage/test", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		settings := d.Config.ImageStorageSettings()
		if !truthyValue(settings["enabled"]) {
			writeErr(c, Errorf(http.StatusBadRequest, "WebDAV 图片存储未启用"))
			return
		}
		result, testErr := service.TestWebDAV(c.Request.Context(), settings)
		if testErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, testErr.Error()))
			return
		}
		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	r.POST("/api/image-storage/sync", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		settings := d.Config.ImageStorageSettings()
		if !truthyValue(settings["enabled"]) {
			writeErr(c, Errorf(http.StatusBadRequest, "WebDAV 图片存储未启用"))
			return
		}
		result, syncErr := d.Images.SyncToWebDAV(c.Request.Context(), settings)
		if syncErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, syncErr.Error()))
			return
		}
		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	// /api/proxy（GET/POST）**刻意不实现**：真实编辑入口是设置页的
	// config-card.tsx → POST /api/settings。历史上曾有两个未被引用的
	// 前端组件指向这里，那些组件与对应的 api.ts 辅助函数已在 2.2.3 清理。
	// 保留 404/405 行为与 Python 版一致。
}
