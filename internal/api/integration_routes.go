package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

// registerIntegrationRoutes 挂载 CPA 与 sub2api 的连接管理端点。
func (d *Deps) registerIntegrationRoutes(r *gin.Engine) {
	d.registerCPARoutes(r)
	d.registerSub2APIRoutes(r)
}

// ---- CPA ----

func (d *Deps) registerCPARoutes(r *gin.Engine) {
	r.GET("/api/cpa/pools", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"pools": service.SanitizeCPAPools(d.CPA.ListPools())})
	})

	r.POST("/api/cpa/pools", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		baseURL := strings.TrimSpace(stringValueOf(body["base_url"]))
		if baseURL == "" {
			writeErr(c, Errorf(http.StatusBadRequest, "base_url is required"))
			return
		}
		secretKey := strings.TrimSpace(stringValueOf(body["secret_key"]))
		if secretKey == "" {
			writeErr(c, Errorf(http.StatusBadRequest, "secret_key is required"))
			return
		}
		pool := d.CPA.AddPool(stringValueOf(body["name"]), baseURL, secretKey)
		c.JSON(http.StatusOK, gin.H{
			"pool":  service.SanitizeCPAPool(pool),
			"pools": service.SanitizeCPAPools(d.CPA.ListPools()),
		})
	})

	r.POST("/api/cpa/pools/:pool_id", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		updates := map[string]any{}
		for _, key := range []string{"name", "base_url", "secret_key"} {
			if value, ok := body[key]; ok && value != nil {
				updates[key] = value
			}
		}
		pool := d.CPA.UpdatePool(c.Param("pool_id"), updates)
		if pool == nil {
			writeErr(c, Errorf(http.StatusNotFound, "pool not found"))
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"pool":  service.SanitizeCPAPool(pool),
			"pools": service.SanitizeCPAPools(d.CPA.ListPools()),
		})
	})

	r.DELETE("/api/cpa/pools/:pool_id", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		if !d.CPA.DeletePool(c.Param("pool_id")) {
			writeErr(c, Errorf(http.StatusNotFound, "pool not found"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"pools": service.SanitizeCPAPools(d.CPA.ListPools())})
	})

	r.GET("/api/cpa/pools/:pool_id/files", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		pool := d.CPA.GetPool(c.Param("pool_id"))
		if pool == nil {
			writeErr(c, Errorf(http.StatusNotFound, "pool not found"))
			return
		}
		files, listErr := service.ListRemoteFiles(c.Request.Context(),
			stringValueOf(pool["base_url"]), stringValueOf(pool["secret_key"]))
		if listErr != nil {
			writeErr(c, Errorf(http.StatusBadGateway, listErr.Error()))
			return
		}
		c.JSON(http.StatusOK, gin.H{"pool_id": c.Param("pool_id"), "files": files})
	})

	r.POST("/api/cpa/pools/:pool_id/import", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		pool := d.CPA.GetPool(c.Param("pool_id"))
		if pool == nil {
			writeErr(c, Errorf(http.StatusNotFound, "pool not found"))
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		names := stringList(body["names"])
		if len(names) == 0 {
			writeErr(c, Errorf(http.StatusBadRequest, "names is required"))
			return
		}
		job := d.startCPAImport(pool, names)
		c.JSON(http.StatusOK, gin.H{"import_job": job})
	})

	r.GET("/api/cpa/pools/:pool_id/import", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		pool := d.CPA.GetPool(c.Param("pool_id"))
		if pool == nil {
			writeErr(c, Errorf(http.StatusNotFound, "pool not found"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"import_job": pool["import_job"]})
	})
}

// startCPAImport 创建导入任务并异步执行。
func (d *Deps) startCPAImport(pool map[string]any, names []string) map[string]any {
	job := map[string]any{
		"job_id":     newUUIDString(),
		"status":     "pending",
		"created_at": nowISO(),
		"updated_at": nowISO(),
		"total":      len(names),
		"completed":  0,
		"added":      0,
		"skipped":    0,
		"refreshed":  0,
		"failed":     0,
		"errors":     []any{},
	}
	poolID := stringValueOf(pool["id"])
	saved := d.CPA.SetImportJob(poolID, job)
	if saved == nil {
		return job
	}
	snapshot, _ := saved["import_job"].(map[string]any)

	// 导入会写 accounts.json，必须能随服务关闭取消。
	importCtx, finishImport, importOK := d.Tasks.StartBackground(contextBackground(), 30*time.Minute)
	if !importOK {
		return snapshot
	}
	go func() {
		defer finishImport()
		added, failed, processed := 0, 0, 0
		errorsList := make([]any, 0)
		for index, name := range names {
			// 每一轮的远程拉取都受 importCtx 约束：服务关闭后不再继续下一个账号。
			if importCtx.Err() != nil {
				break
			}
			processed = index + 1
			token, fetchErr := service.FetchRemoteAccessToken(
				importCtx, stringValueOf(pool["base_url"]),
				stringValueOf(pool["secret_key"]), name)
			if fetchErr != nil {
				failed++
				errorsList = append(errorsList, map[string]any{"name": name, "error": fetchErr.Error()})
			} else {
				d.Accts.AddAccountItems(nil, []string{token})
				added++
			}
			d.CPA.SetImportJob(poolID, map[string]any{
				"job_id":     job["job_id"],
				"status":     "running",
				"created_at": job["created_at"],
				"updated_at": nowISO(),
				"total":      len(names),
				"completed":  index + 1,
				"added":      added,
				"skipped":    0,
				"refreshed":  0,
				"failed":     failed,
				"errors":     errorsList,
			})
		}
		status := "success"
		if failed > 0 && added == 0 {
			status = "failed"
		}
		// 被关闭取消时 completed 只能报真实处理过的数量，不能写 len(names) 假装跑完。
		if processed < len(names) && status == "success" {
			status = "failed"
			errorsList = append(errorsList, map[string]any{
				"name": "import", "error": "import cancelled: server is shutting down",
			})
		}
		d.CPA.SetImportJob(poolID, map[string]any{
			"job_id":     job["job_id"],
			"status":     status,
			"created_at": job["created_at"],
			"updated_at": nowISO(),
			"total":      len(names),
			"completed":  processed,
			"added":      added,
			"skipped":    0,
			"refreshed":  0,
			"failed":     failed,
			"errors":     errorsList,
		})
	}()

	return snapshot
}

// ---- sub2api ----

func (d *Deps) registerSub2APIRoutes(r *gin.Engine) {
	r.GET("/api/sub2api/servers", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"servers": service.SanitizeSub2APIServers(d.Sub2API.ListServers())})
	})

	r.POST("/api/sub2api/servers", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		baseURL := strings.TrimSpace(stringValueOf(body["base_url"]))
		if baseURL == "" {
			writeErr(c, Errorf(http.StatusBadRequest, "base_url is required"))
			return
		}
		email := strings.TrimSpace(stringValueOf(body["email"]))
		password := strings.TrimSpace(stringValueOf(body["password"]))
		apiKey := strings.TrimSpace(stringValueOf(body["api_key"]))
		if !(email != "" && password != "") && apiKey == "" {
			writeErr(c, Errorf(http.StatusBadRequest, "email+password or api_key is required"))
			return
		}
		server := d.Sub2API.AddServer(stringValueOf(body["name"]), baseURL, email, password, apiKey, stringValueOf(body["group_id"]))
		c.JSON(http.StatusOK, gin.H{
			"server":  service.SanitizeSub2APIServer(server),
			"servers": service.SanitizeSub2APIServers(d.Sub2API.ListServers()),
		})
	})

	r.POST("/api/sub2api/servers/:server_id", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		updates := map[string]any{}
		for _, key := range []string{"name", "base_url", "email", "password", "api_key", "group_id"} {
			if value, ok := body[key]; ok && value != nil {
				updates[key] = value
			}
		}
		server := d.Sub2API.UpdateServer(c.Param("server_id"), updates)
		if server == nil {
			writeErr(c, Errorf(http.StatusNotFound, "server not found"))
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"server":  service.SanitizeSub2APIServer(server),
			"servers": service.SanitizeSub2APIServers(d.Sub2API.ListServers()),
		})
	})

	r.DELETE("/api/sub2api/servers/:server_id", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		if !d.Sub2API.DeleteServer(c.Param("server_id")) {
			writeErr(c, Errorf(http.StatusNotFound, "server not found"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"servers": service.SanitizeSub2APIServers(d.Sub2API.ListServers())})
	})

	r.GET("/api/sub2api/servers/:server_id/groups", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		server := d.Sub2API.GetServer(c.Param("server_id"))
		if server == nil {
			writeErr(c, Errorf(http.StatusNotFound, "server not found"))
			return
		}
		groups, listErr := d.Sub2API.ListRemoteGroups(c.Request.Context(), server)
		if listErr != nil {
			writeErr(c, Errorf(http.StatusBadGateway, listErr.Error()))
			return
		}
		c.JSON(http.StatusOK, gin.H{"server_id": c.Param("server_id"), "groups": groups})
	})

	r.GET("/api/sub2api/servers/:server_id/accounts", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		server := d.Sub2API.GetServer(c.Param("server_id"))
		if server == nil {
			writeErr(c, Errorf(http.StatusNotFound, "server not found"))
			return
		}
		accounts, listErr := d.Sub2API.ListRemoteAccounts(c.Request.Context(), server)
		if listErr != nil {
			writeErr(c, Errorf(http.StatusBadGateway, listErr.Error()))
			return
		}
		c.JSON(http.StatusOK, gin.H{"server_id": c.Param("server_id"), "accounts": accounts})
	})

	r.POST("/api/sub2api/servers/:server_id/import", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		server := d.Sub2API.GetServer(c.Param("server_id"))
		if server == nil {
			writeErr(c, Errorf(http.StatusNotFound, "server not found"))
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		accountIDs := stringList(body["account_ids"])
		if len(accountIDs) == 0 {
			writeErr(c, Errorf(http.StatusBadRequest, "account ids is required"))
			return
		}
		job := d.startSub2APIImport(server, accountIDs)
		c.JSON(http.StatusOK, gin.H{"import_job": job})
	})

	r.GET("/api/sub2api/servers/:server_id/import", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		server := d.Sub2API.GetServer(c.Param("server_id"))
		if server == nil {
			writeErr(c, Errorf(http.StatusNotFound, "server not found"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"import_job": server["import_job"]})
	})
}

// startSub2APIImport 创建 sub2api 导入任务并异步执行。
func (d *Deps) startSub2APIImport(server map[string]any, accountIDs []string) map[string]any {
	job := map[string]any{
		"job_id":     newUUIDString(),
		"status":     "pending",
		"created_at": nowISO(),
		"updated_at": nowISO(),
		"total":      len(accountIDs),
		"completed":  0,
		"added":      0,
		"skipped":    0,
		"refreshed":  0,
		"failed":     0,
		"errors":     []any{},
	}
	serverID := stringValueOf(server["id"])
	saved := d.Sub2API.SetImportJob(serverID, job)
	if saved == nil {
		return job
	}
	snapshot, _ := saved["import_job"].(map[string]any)

	// sub2api 导入同样会写 accounts.json。
	importCtx, finishImport, importOK := d.Tasks.StartBackground(contextBackground(), 30*time.Minute)
	if !importOK {
		return snapshot
	}
	go func() {
		defer finishImport()
		ctx := importCtx
		tokens, failures, fetchErr := d.Sub2API.FetchAccessTokens(ctx, server, accountIDs)
		errorsList := make([]any, 0, len(failures))
		for _, failure := range failures {
			errorsList = append(errorsList, failure)
		}

		added := 0
		if fetchErr == nil && len(tokens) > 0 {
			result := d.Accts.AddAccountItems(nil, tokens)
			added = intOf(result["added"])
		}

		status := "success"
		if fetchErr != nil {
			status = "failed"
			errorsList = append(errorsList, map[string]any{"name": "fetch", "error": fetchErr.Error()})
		} else if added == 0 && len(errorsList) > 0 {
			status = "failed"
		}

		d.Sub2API.SetImportJob(serverID, map[string]any{
			"job_id":     job["job_id"],
			"status":     status,
			"created_at": job["created_at"],
			"updated_at": nowISO(),
			"total":      len(accountIDs),
			"completed":  len(accountIDs),
			"added":      added,
			"skipped":    len(accountIDs) - added,
			"refreshed":  0,
			"failed":     len(errorsList),
			"errors":     errorsList,
		})
	}()

	return snapshot
}
