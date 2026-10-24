package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// maskAccountCredentials 把账号记录里的密码类字段替换成「是否已设置」标记。
//
// 为什么要脱敏：密码重登上线后 password / totp_secret 变成有实际作用的凭据，
// 而 /api/accounts 是列表接口 —— 一次只读的 admin 会话就能拿到整个号池的
// 密码与 TOTP 种子，等于号池全量接管。access_token 保留原文是既有约定
// （前端要显示、导出要用），密码没有这个需求。
//
// 保留 *_set 布尔值，让前端能显示「已配置密码 / 未配置」。
func maskAccountCredentials(items []map[string]any) []map[string]any {
	for _, item := range items {
		for _, field := range []string{"password", "totp_secret"} {
			value := stringValueOf(item[field])
			if value == "" {
				delete(item, field)
				continue
			}
			delete(item, field)
			item[field+"_set"] = true
		}
	}
	return items
}

// registerAccountCRUD 复刻 api/accounts.py 里 /api/accounts 的六个端点。
func (d *Deps) registerAccountCRUD(r *gin.Engine) {
	r.GET("/api/accounts", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": maskAccountCredentials(d.Accts.ListAccounts())})
	})

	r.POST("/api/accounts", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		tokens, accounts, payloadErr := parseAccountCreateBody(body)
		if payloadErr != nil {
			writeErr(c, payloadErr)
			return
		}
		if len(tokens) == 0 && len(accounts) == 0 {
			writeErr(c, Errorf(http.StatusBadRequest, "tokens is required"))
			return
		}
		result := d.Accts.AddAccountItems(accounts, tokens)

		// 导入后立刻同步刷新一次，与 Python api/accounts.py:233 一致。
		//
		// 不刷新的话，新导入账号的 email/quota/limits_progress 全是空，
		// 而前端 toast 会提示「已自动刷新账号信息」—— UI 说了假话，
		// 用户以为数据就绪，直到手动点刷新才发现是空的。
		allTokens := make([]string, 0, len(tokens)+len(accounts))
		allTokens = append(allTokens, tokens...)
		for _, account := range accounts {
			if token := strings.TrimSpace(stringValueOf(account["access_token"])); token != "" {
				allTokens = append(allTokens, token)
			}
		}

		if len(allTokens) > 0 {
			refreshResult := d.Accts.RefreshAccounts(
				c.Request.Context(), allTokens, "", d.refreshVerifier())
			result["refreshed"] = refreshResult["refreshed"]
			result["errors"] = refreshResult["errors"]
			if items, ok := refreshResult["items"]; ok {
				result["items"] = items
			}
		} else {
			result["refreshed"] = 0
			result["errors"] = []any{}
		}

		c.JSON(http.StatusOK, result)
	})

	r.DELETE("/api/accounts", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		tokens := stringList(body["tokens"])
		if len(tokens) == 0 {
			writeErr(c, Errorf(http.StatusBadRequest, "tokens is required"))
			return
		}
		c.JSON(http.StatusOK, d.Accts.DeleteAccounts(tokens))
	})

	r.POST("/api/accounts/update", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		accessToken := strings.TrimSpace(stringValueOf(body["access_token"]))
		if accessToken == "" {
			writeErr(c, Errorf(http.StatusBadRequest, "access_token is required"))
			return
		}
		updates := map[string]any{}
		for _, key := range []string{"type", "status", "quota", "proxy"} {
			if value, ok := body[key]; ok && value != nil {
				updates[key] = value
			}
		}
		if len(updates) == 0 {
			writeErr(c, Errorf(http.StatusBadRequest, "还没有检测到改动，请修改后再保存"))
			return
		}
		account := d.Accts.UpdateAccount(accessToken, updates, false)
		if account == nil {
			writeErr(c, Errorf(http.StatusNotFound, "account not found"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"item": account, "items": d.Accts.ListAccounts()})
	})
}

// parseAccountCreateBody 复刻 Python 的入参归一化：
// tokens 是字符串数组，accounts 是对象数组，两者可同时存在，token 去重。
func parseAccountCreateBody(body map[string]any) ([]string, []map[string]any, *HTTPError) {
	rawTokens, tokensIsList := body["tokens"]
	_ = rawTokens
	if value, exists := body["tokens"]; exists && value != nil {
		if _, ok := value.([]any); !ok {
			if _, ok := value.([]string); !ok {
				// Pydantic 对非列表返回 422 + list_type；这里保持同形状。
				return nil, nil, ErrorDetail(http.StatusUnprocessableEntity, []any{
					map[string]any{
						"type":  "list_type",
						"loc":   []any{"body", "tokens"},
						"msg":   "Input should be a valid list",
						"input": value,
					},
				})
			}
		}
	}
	_ = tokensIsList

	accounts := make([]map[string]any, 0)
	if rawAccounts, ok := body["accounts"].([]any); ok {
		for _, item := range rawAccounts {
			if account, ok := item.(map[string]any); ok {
				accounts = append(accounts, account)
			}
		}
	}

	seen := map[string]bool{}
	merged := make([]string, 0)
	appendToken := func(token string) {
		trimmed := strings.TrimSpace(token)
		if trimmed == "" || seen[trimmed] {
			return
		}
		seen[trimmed] = true
		merged = append(merged, trimmed)
	}
	for _, token := range stringList(body["tokens"]) {
		appendToken(token)
	}
	for _, account := range accounts {
		if token := strings.TrimSpace(stringValueOf(account["access_token"])); token != "" {
			appendToken(token)
			continue
		}
		if token := strings.TrimSpace(stringValueOf(account["accessToken"])); token != "" {
			appendToken(token)
		}
	}
	return merged, accounts, nil
}

// stringList 把任意 JSON 数组转成去空字符串列表。
func stringList(value any) []string {
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(stringValueOf(item)); text != "" {
				out = append(out, text)
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(item); text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}
