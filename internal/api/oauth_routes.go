package api

import (
	"net/http"

	"github.com/IFuZYI/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

// registerOAuthRoutes 挂载手动 OAuth 桥（PKCE 授权码流程）。
func (d *Deps) registerOAuthRoutes(r *gin.Engine) {
	r.POST("/api/accounts/oauth/start", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		result, startErr := d.OAuth.Start(stringValueOf(body["email_hint"]))
		if startErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, startErr.Error()))
			return
		}
		c.JSON(http.StatusOK, result)
	})

	r.POST("/api/accounts/oauth/finish", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		tokens, finishErr := d.OAuth.Finish(c.Request.Context(),
			stringValueOf(body["session_id"]), stringValueOf(body["callback"]))
		if finishErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, finishErr.Error()))
			return
		}

		payload := map[string]any{
			"access_token":  tokens["access_token"],
			"refresh_token": tokens["refresh_token"],
			"id_token":      tokens["id_token"],
			"source_type":   "oauth_login",
		}
		addResult := d.Accts.AddAccountItems([]map[string]any{payload}, nil)

		// 立刻刷新一次拿账号信息（失败不影响导入结果）。
		if d.AccountInfoFetcher != nil {
			if info, infoErr := d.AccountInfoFetcher(c.Request.Context(), tokens["access_token"]); infoErr == nil {
				d.Accts.UpdateAccount(tokens["access_token"], info, true)
			}
		}

		refreshed := 1
		if intOf(addResult["added"]) == 0 {
			refreshed = 1 // 已存在也算刷新过
		}
		// items 必须脱敏：AddAccountItems 返回的是号池原始记录，
		// 实测含 password / totp_secret 原文。
		c.JSON(http.StatusOK, gin.H{
			"added":     addResult["added"],
			"skipped":   addResult["skipped"],
			"refreshed": refreshed,
			"errors":    []any{},
			"items":     maskItems(addResult["items"]),
		})
	})
}

// 保持 service 引用（OAuthLoginService 类型来自 service）。
var _ = service.RoleAdmin
