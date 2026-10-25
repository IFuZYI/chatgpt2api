package api

import (
	"net/http"

	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/gin-gonic/gin"
)

// RegisterV1Routes 注册 /v1/* 兼容接口。
//
// M3 先落地 /v1/models；chat/responses/messages 在后续步骤接入，
// 它们的信封与鉴权已在 deps.go / errors.go 里就位。
func (d *Deps) RegisterV1Routes(r *gin.Engine) {
	r.GET("/v1/models", func(c *gin.Context) {
		if _, err := d.RequireIdentity(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}

		ctx := c.Request.Context()
		upstreamModels := d.Models.ListModels(ctx)

		accounts := d.Accts.ListAccounts()
		summaries := make([]protocol.AccountSummary, 0, len(accounts))
		for _, account := range accounts {
			summaries = append(summaries, protocol.AccountSummary{
				Type:       stringValueOf(account["type"]),
				SourceType: stringValueOf(account["source_type"]),
			})
		}

		c.JSON(http.StatusOK, protocol.ListModelsResult(upstreamModels, summaries))
	})
}
