package api

import (
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

// RegisterAuthRoutes 注册 /api/auth/users 四个方法。
// 全部要求 admin（Python 里也是 require_admin）。
func (d *Deps) RegisterAuthRoutes(r *gin.Engine) {
	r.GET("/api/auth/users", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": d.Auth.ListKeys(service.RoleUser)})
	})

	r.POST("/api/auth/users", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		name := strings.TrimSpace(stringValueOf(body["name"]))
		item, rawKey, createErr := d.Auth.CreateKey(service.RoleUser, name)
		if createErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, createErr.Error()))
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"item":  item,
			"key":   rawKey,
			"items": d.Auth.ListKeys(service.RoleUser),
		})
	})

	r.POST("/api/auth/users/:key_id", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		updates := map[string]any{}
		for _, key := range []string{"name", "enabled", "key"} {
			if value, ok := body[key]; ok && value != nil {
				updates[key] = value
			}
		}
		if len(updates) == 0 {
			writeErr(c, Errorf(http.StatusBadRequest, "还没有检测到改动，请修改后再保存"))
			return
		}
		item, updateErr := d.Auth.UpdateKey(c.Param("key_id"), updates, service.RoleUser)
		if updateErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, updateErr.Error()))
			return
		}
		if item == nil {
			writeErr(c, Errorf(http.StatusNotFound, "这条用户密钥不存在，可能已经被删除"))
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"item":  item,
			"items": d.Auth.ListKeys(service.RoleUser),
		})
	})

	r.DELETE("/api/auth/users/:key_id", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		if !d.Auth.DeleteKey(c.Param("key_id"), service.RoleUser) {
			writeErr(c, Errorf(http.StatusNotFound, "这条用户密钥不存在，可能已经被删除"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": d.Auth.ListKeys(service.RoleUser)})
	})
}
