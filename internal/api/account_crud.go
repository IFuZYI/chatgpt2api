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

		// 刷新默认**不做**，由 ?refresh=1 显式开启。
		//
		// 为什么默认关：同步刷新会给每个账号打一次上游（/me + conversation/init
		// + accounts/check），注册机一次传 500 个账号要串行等几分钟，且
		// 上游会看到 500 个新会话。上传接口的职责是「落盘并返回计数」，
		// 刷新交给后台 watcher（启动后先跑一轮，之后按
		// refresh_account_interval_minute 周期跑）。
		//
		// 需要立即验证 token 有效性的调用方（例如注册机自己要先筛掉死号）
		// 传 ?refresh=1 即可恢复旧行为。
		// 去重：同一个 token 可能同时出现在 tokens 与 accounts 里
		// （parseAccountCreateBody 已为 tokens 生成过 account 条目）。
		// 不去重会让 refresh_deferred 翻倍 —— 实测上传 1 个账号报 2。
		seen := make(map[string]bool, len(tokens)+len(accounts))
		allTokens := make([]string, 0, len(tokens)+len(accounts))
		addToken := func(raw string) {
			token := strings.TrimSpace(raw)
			if token == "" || seen[token] {
				return
			}
			seen[token] = true
			allTokens = append(allTokens, token)
		}
		for _, token := range tokens {
			addToken(token)
		}
		for _, account := range accounts {
			addToken(stringValueOf(account["access_token"]))
		}
		result["refreshed"] = 0
		result["errors"] = []any{}
		result["refresh_deferred"] = len(allTokens)

		if wantsRefresh(c) && len(allTokens) > 0 {
			refreshResult := d.Accts.RefreshAccounts(
				c.Request.Context(), allTokens, "", d.refreshVerifier())
			result["refreshed"] = refreshResult["refreshed"]
			result["errors"] = refreshResult["errors"]
			result["refresh_deferred"] = 0
			if items, ok := refreshResult["items"]; ok {
				result["items"] = items
			}
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

	// 批量设置代理：一次给多个账号分配出口代理。
	//
	// 注册机通常按批产出账号，逐个调 /api/accounts/update 既慢又容易漏。
	// 支持两种形态：
	//   1. 统一代理：{"access_tokens":[...], "proxy":"socks5h://..."}
	//   2. 逐个映射：{"proxies":{"at-1":"socks5h://a", "at-2":""}}
	//      （映射里的空字符串表示清空该账号的代理，回退全局）
	r.POST("/api/accounts/proxy", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}

		assignments := map[string]string{}
		if raw, ok := body["proxies"].(map[string]any); ok {
			for token, value := range raw {
				assignments[strings.TrimSpace(token)] = strings.TrimSpace(stringValueOf(value))
			}
		}
		// 统一代理形态：access_tokens 里每个账号都设成同一个 proxy。
		if proxy, ok := body["proxy"]; ok {
			shared := strings.TrimSpace(stringValueOf(proxy))
			for _, token := range stringList(body["access_tokens"]) {
				assignments[strings.TrimSpace(token)] = shared
			}
		}
		if len(assignments) == 0 {
			writeErr(c, Errorf(http.StatusBadRequest,
				"需要 access_tokens + proxy，或 proxies 映射"))
			return
		}

		updated := 0
		missing := make([]string, 0)
		for token, proxy := range assignments {
			if token == "" {
				continue
			}
			if proxy == "" {
				// 清空：写空串让 ProxyFor 回退全局。
				if d.Accts.UpdateAccount(token, map[string]any{"proxy": ""}, true) != nil {
					updated++
				} else {
					missing = append(missing, token)
				}
				continue
			}
			if d.Accts.UpdateAccount(token, map[string]any{"proxy": proxy}, true) != nil {
				updated++
			} else {
				missing = append(missing, token)
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"updated": updated,
			"missing": missing,
			"items":   d.Accts.ListAccounts(),
		})
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

// wantsRefresh 报告调用方是否要求上传后同步刷新。
//
// 接受 ?refresh=1 与 ?refresh=true（大小写不敏感）。默认不刷新 ——
// 注册机批量上传时同步刷新会阻塞数分钟，且上游会看到成百个新会话。
func wantsRefresh(c *gin.Context) bool {
	switch strings.ToLower(strings.TrimSpace(c.Query("refresh"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}
