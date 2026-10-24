package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/gin-gonic/gin"
)

// SearchUpstreamFactory 为一次搜索请求构造上游客户端。
// 由 main 注入（与 TextUpstream/ImageUpstream 同理：api 不依赖 upstream）。
type SearchUpstreamFactory func(ctx context.Context, accessToken string) (protocol.SearchUpstream, func(), error)

// registerSearchRoute 挂载 /v1/search（复刻 services/protocol/openai_search.py）。
//
// 请求契约（Python SearchRequest）：只有 prompt 一个字段，min_length=1。
// 模型是固定常量 gpt-5-5，body 里的 model 不参与路由也不参与日志。
func (d *Deps) registerSearchRoute(r *gin.Engine) {
	r.POST("/v1/search", func(c *gin.Context) {
		identity, authErr := d.RequireIdentityCtx(c)
		if authErr != nil {
			writeErr(c, authErr)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}

		prompt := strings.TrimSpace(stringValueOf(body["prompt"]))
		if prompt == "" {
			// Pydantic 的 min_length=1 在 handler 之前就返回 422，
			// 因此这条路径不产生 call 日志（与 Python 一致）。
			writeErr(c, validationError("prompt", "Field required", nil))
			return
		}

		call := d.beginCall("网页搜索", c.FullPath(), protocol.SearchModel, identity.Name)

		// Python 在路由层用 filter_or_log 查内容，失败会记一条失败日志。
		if filterErr := d.checkContent(c.Request.Context(), prompt); filterErr != nil {
			call.finish(filterErr, "", nil)
			writeErr(c, contentFilterError(filterErr))
			return
		}

		result, accountEmail, searchErr := d.runSearch(c.Request.Context(), prompt)
		if searchErr != nil {
			// Python 的失败日志没有 account_email（搜索错误对象上没这个字段），
			// 这里也不传，保持日志字段一致。
			call.finish(searchErr, "", nil)
			writeErr(c, mapProtocolError(searchErr))
			return
		}

		call.finish(nil, accountEmail, nil)
		// Python handle() 把账号邮箱塞进结果（_account_email）。
		result["_account_email"] = accountEmail
		c.JSON(http.StatusOK, result)
	})
}

// runSearch 复刻 openai_search.handle（services/protocol/openai_search.py:7-19）：
// 取文本号 → 建上游客户端 → 搜索 → 成功后 MarkTextUsed。
func (d *Deps) runSearch(ctx context.Context, prompt string) (map[string]any, string, error) {
	if d.SearchUpstream == nil {
		return nil, "", fmt.Errorf("search upstream factory is not configured")
	}

	// 等价 Python 的 get_text_access_token()：默认 model="auto" → 不做模型路由，
	// 号池为空时返回空 token（匿名），由上游自己报 access_token is required。
	token, err := d.Accts.GetTextAccessToken(nil, nil, true)
	if err != nil {
		return nil, "", err
	}

	accountEmail := ""
	if account := d.Accts.GetAccount(token); account != nil {
		accountEmail = stringValueOf(account["email"])
	}

	upstreamClient, closeFn, err := d.SearchUpstream(ctx, token)
	if err != nil {
		return nil, accountEmail, err
	}
	defer closeFn()

	result, err := upstreamClient.Search(ctx, prompt, protocol.SearchModel)
	if err != nil {
		return nil, accountEmail, err
	}
	// 只有成功才记账（Python 的 mark_text_used 在 search 之后调用）。
	if token != "" {
		d.Accts.MarkTextUsed(token)
	}
	return result, accountEmail, nil
}
