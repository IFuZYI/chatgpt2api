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
//
// 覆盖 5 个凭据字段，不只是 password / totp_secret：
// refresh_token / id_token / session_token 同样是可直接接管账号的凭据。
//
// **不修改入参**：ListAccounts 返回的是 copyMap 副本，但同一批 map 可能被
// 其他路径复用；就地 delete 会删掉号池内存里的密码，导致后续重登失败。
// 所以每个 item 复制一份再处理。
func maskAccountCredentials(items []map[string]any) []map[string]any {
	secretFields := []string{"password", "totp_secret", "refresh_token", "id_token", "session_token"}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		masked := make(map[string]any, len(item)+len(secretFields))
		for key, value := range item {
			masked[key] = value
		}
		for _, field := range secretFields {
			value := stringValueOf(masked[field])
			delete(masked, field)
			if value != "" {
				masked[field+"_set"] = true
			}
		}
		out = append(out, masked)
	}
	return out
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

	// 单账号详情：返回**未脱敏**的完整记录 + 派生字段。
	//
	// 与 GET /api/accounts 的差异是刻意的：列表是批量读，一次会话就能拿到
	// 整个号池的密码与 TOTP 种子，所以那里必须脱敏；详情是显式的单条查询，
	// 用户点「查看」要的就是凭据原文。因此这里：
	//   - 只认 admin（user key 连列表都读不到，详情更不能开）
	//   - 必须带 access_token 参数，不支持「不传就返回全部」
	//   - 账号不存在返回 404，与「参数缺失」的 400 区分开
	r.GET("/api/accounts/detail", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		lookup, ok := resolveAccountLookup(c)
		if !ok {
			writeErr(c, Errorf(http.StatusBadRequest,
				"access_token 或 account_id 必填一个"))
			return
		}
		detail := d.Accts.Detail(lookup)
		if detail == nil {
			writeErr(c, Errorf(http.StatusNotFound, "account not found"))
			return
		}
		// 详情含凭据原文，禁止任何中间层缓存。
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"item": detail})
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

		// 响应里的 items 必须脱敏。AddAccountItems / RefreshAccounts 返回的是
		// 号池原始记录，实测含 password 与 totp_secret 原文。
		result["items"] = maskItems(result["items"])

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
		// 删除接口返回「剩余账号列表」供前端刷新表格 —— 它同样是一次
		// 批量读，必须脱敏（实测未脱敏时 260 个账号全部带凭据原文）。
		deleted := d.Accts.DeleteAccounts(tokens)
		deleted["items"] = maskItems(deleted["items"])
		c.JSON(http.StatusOK, deleted)
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
		// totp_secret / password 也允许改：注册机常先传 token，
		// 事后才发现账号开了 2FA 需要补 TOTP；要求「重新上传整个账号对象」
		// 既笨拙又容易把已有字段覆盖成空。
		updates := map[string]any{}
		for _, key := range []string{"type", "status", "quota", "proxy", "password", "totp_secret", "email"} {
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
		// 列表与单项都脱敏：update 之后前端会把 items 存进状态，
		// 返回凭据原文等于把密码送进浏览器内存。
		c.JSON(http.StatusOK, gin.H{
			"item":  maskAccountCredentials([]map[string]any{account})[0],
			"items": maskAccountCredentials(d.Accts.ListAccounts()),
		})
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
			"items":   maskAccountCredentials(d.Accts.ListAccounts()),
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

// resolveAccountLookup 从查询参数里取出要查的账号标识。
//
// 按优先级取 access_token → account_id → id（`id` 是 account_id 的简写）。
// access_token 优先是为了兼容既有前端调用；它同时也是所有上游请求需要的键。
//
// 为什么还要支持 account_id：access_token 会被轮换（RT 换 AT、密码重登），
// 轮换后旧 token 查不到账号（aliases 只在内存里，重启即失效）。
// account_id 终身不变，外部系统应当用它做引用。
func resolveAccountLookup(c *gin.Context) (string, bool) {
	for _, key := range []string{"access_token", "account_id", "id"} {
		if value := strings.TrimSpace(c.Query(key)); value != "" {
			return value, true
		}
	}
	return "", false
}

// maskItems 把任意形态的 items 值（[]map[string]any 或 []any）脱敏。
//
// 抽出来是因为 service 层返回的 result["items"] 可能是两种类型：
// 直接构造的 []map[string]any，或经 JSON 往返后的 []any。
// 三处端点各自写一遍类型断言太容易漏 —— 事实上有两个端点就漏了。
func maskItems(value any) []map[string]any {
	switch typed := value.(type) {
	case []map[string]any:
		return maskAccountCredentials(typed)
	case []any:
		converted := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if entry, ok := item.(map[string]any); ok {
				converted = append(converted, entry)
			}
		}
		return maskAccountCredentials(converted)
	}
	return nil
}
