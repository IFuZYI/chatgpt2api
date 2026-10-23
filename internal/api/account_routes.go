package api

import (
	"github.com/gin-gonic/gin"
)

// RegisterAccountRoutes 注册 /api/accounts 与 /api/cpa、/api/sub2api。
func (d *Deps) RegisterAccountRoutes(r *gin.Engine) {
	d.registerAccountCRUD(r)
	d.registerAccountRefresh(r)
	d.registerAccountExport(r)
	d.registerOAuthRoutes(r)
}
