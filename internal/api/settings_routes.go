package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/service"
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
		c.JSON(http.StatusOK, gin.H{"config": d.Config.Get()})
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
		c.JSON(http.StatusOK, gin.H{"config": updated})
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

	// 前端 api.ts 里 fetchProxy/updateProxy 指向 /api/proxy，但这两个函数只被
	// 两个没有任何地方 import 的孤儿组件使用（实测），真实编辑入口是
	// config-card.tsx 的输入框 → POST /api/settings。这里不补路由（见计划书修正 1），
	// 保持与 Python 版相同的 404/405 行为。
	_ = strings.TrimSpace
}
