package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

// backupSettings 把配置转成 service 需要的结构。
func (d *Deps) backupSettings() service.BackupSettings {
	settings := d.Config.BackupSettings()
	include, _ := settings["include"].(map[string]any)
	return service.BackupSettings{
		Enabled:      truthyValue(settings["enabled"]),
		AccountID:    strings.TrimSpace(stringValueOf(settings["account_id"])),
		AccessKeyID:  strings.TrimSpace(stringValueOf(settings["access_key_id"])),
		SecretKey:    strings.TrimSpace(stringValueOf(settings["secret_access_key"])),
		Bucket:       strings.TrimSpace(stringValueOf(settings["bucket"])),
		Prefix:       strings.TrimSpace(stringValueOf(settings["prefix"])),
		IntervalMin:  intOf(settings["interval_minutes"]),
		RotationKeep: intOf(settings["rotation_keep"]),
		Encrypt:      truthyValue(settings["encrypt"]),
		Passphrase:   strings.TrimSpace(stringValueOf(settings["passphrase"])),
		Include:      include,
	}
}

// registerBackupRoutes 挂载备份相关端点。
func (d *Deps) registerBackupRoutes(r *gin.Engine) {
	if d.backupStateStore == nil {
		d.backupStateStore = newBackupStateStore(d.Config.BackupStateFile())
	}
	r.POST("/api/backup/test", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		client := service.NewR2Client(d.backupSettings())
		result, testErr := client.TestConnection(c.Request.Context())
		if testErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, testErr.Error()))
			return
		}
		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	r.GET("/api/backups", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		settings := d.backupSettings()
		// 未启用或凭据不全时返回空列表，不发起网络请求。
		// 仓库默认 config.json 里 account_id 是占位值 "admin"、bucket 为空，
		// 只判空字符串会把占位配置当成真配置去连 R2（实测会拿回 HTML 错误页）。
		if !settings.Enabled || settings.Bucket == "" || settings.AccessKeyID == "" {
			c.JSON(http.StatusOK, gin.H{
				"items":    []any{},
				"state":    d.backupState(),
				"settings": d.Config.BackupSettings(),
			})
			return
		}
		client := service.NewR2Client(settings)
		objects, listErr := client.ListObjects(c.Request.Context())
		if listErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, listErr.Error()))
			return
		}
		items := make([]map[string]any, 0, len(objects))
		for _, object := range objects {
			if !service.IsBackupObject(stringValueOf(object["key"])) {
				continue
			}
			items = append(items, service.BackupSummary(object))
		}
		c.JSON(http.StatusOK, gin.H{
			"items":    items,
			"state":    d.backupState(),
			"settings": d.Config.BackupSettings(),
		})
	})

	r.POST("/api/backups/run", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		// 已有备份在跑时拒绝（Python backup_service.py:471-473 同样行为）。
		if !d.backupStateStore.Begin() {
			writeErr(c, Errorf(http.StatusBadRequest, "当前已有备份任务正在执行"))
			return
		}
		d.backupStateStore.MarkStarted()

		result, runErr := d.runBackup(c.Request.Context())
		if runErr != nil {
			d.backupStateStore.End("error", "", runErr.Error())
			writeErr(c, Errorf(http.StatusBadRequest, runErr.Error()))
			return
		}
		objectKey, _ := result["key"].(string)
		d.backupStateStore.End("success", objectKey, "")
		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	r.POST("/api/backups/delete", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		key := strings.TrimSpace(stringValueOf(body["key"]))
		if key == "" {
			writeErr(c, Errorf(http.StatusBadRequest, "备份对象 key 不能为空"))
			return
		}
		client := service.NewR2Client(d.backupSettings())
		if err := client.DeleteObject(c.Request.Context(), key); err != nil {
			writeErr(c, Errorf(http.StatusBadRequest, err.Error()))
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	r.GET("/api/backups/detail", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		key := strings.TrimSpace(c.Query("key"))
		if key == "" {
			writeErr(c, Errorf(http.StatusBadRequest, "备份对象 key 不能为空"))
			return
		}
		detail, detailErr := d.backupDetail(c.Request.Context(), key)
		if detailErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, detailErr.Error()))
			return
		}
		c.JSON(http.StatusOK, gin.H{"item": detail})
	})

	r.GET("/api/backups/download", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		key := strings.TrimSpace(c.Query("key"))
		if key == "" {
			writeErr(c, Errorf(http.StatusBadRequest, "备份对象 key 不能为空"))
			return
		}
		item, downloadErr := d.backupDownload(c.Request.Context(), key)
		if downloadErr != nil {
			writeErr(c, Errorf(http.StatusBadRequest, downloadErr.Error()))
			return
		}
		name := stringValueOf(item["name"])
		c.Header("Content-Disposition", `attachment; filename*=UTF-8''`+encodeURIPath(name))
		c.Header("Content-Length", itoa(len(item["payload"].([]byte))))
		c.Data(http.StatusOK, stringValueOf(item["content_type"]), item["payload"].([]byte))
	})
}

// runBackup 构建归档 → 可选加密 → 上传 → 轮转。
func (d *Deps) runBackup(ctx context.Context) (map[string]any, error) {
	settings := d.backupSettings()
	client := service.NewR2Client(settings)
	if err := client.Validate(); err != nil {
		return nil, err
	}

	builder := &service.BackupBuilder{
		ConfigPath:         d.Config.Path(),
		DataDir:            d.Config.DataDir(),
		Version:            d.Config.AppVersion(),
		StorageBackendInfo: d.Storage.BackendInfo(),
		// 必须注入：不注入时 Build() 里 `if s.Accounts != nil` 恒 false，
		// snapshots/accounts.json 与 auth_keys.json 永远不写入 ——
		// 备份能成功、能恢复，但恢复出来没有任何账号和密钥。
		Accounts: d.Accts,
		Auth:     d.Auth,
	}
	archive, buildErr := builder.Build(settings, "manual")
	if buildErr != nil {
		return nil, buildErr
	}
	payload, encodeErr := archive.Encode()
	if encodeErr != nil {
		return nil, encodeErr
	}

	if settings.Encrypt {
		if settings.Passphrase == "" {
			return nil, service.NewBackupConfigError("已启用备份加密，但未设置加密口令")
		}
		encrypted, encryptErr := service.EncryptOpenSSL(payload, settings.Passphrase)
		if encryptErr != nil {
			return nil, encryptErr
		}
		payload = encrypted
	}

	key := service.BackupObjectKey(settings.Prefix, time.Now(), service.RandomHexTag(), settings.Encrypt)
	metadata := map[string]string{
		"created-at": time.Now().UTC().Format(time.RFC3339Nano),
		"encrypted":  map[bool]string{true: "true", false: "false"}[settings.Encrypt],
		"trigger":    "manual",
	}
	if _, uploadErr := client.UploadBytes(ctx, key, payload, "application/octet-stream", metadata); uploadErr != nil {
		return nil, uploadErr
	}

	// 轮转：保留最新的 rotation_keep 个。
	if settings.RotationKeep > 0 {
		if objects, listErr := client.ListObjects(ctx); listErr == nil {
			backups := make([]string, 0, len(objects))
			for _, object := range objects {
				if candidate := stringValueOf(object["key"]); service.IsBackupObject(candidate) {
					backups = append(backups, candidate)
				}
			}
			for index := settings.RotationKeep; index < len(backups); index++ {
				_ = client.DeleteObject(ctx, backups[index])
			}
		}
	}

	return map[string]any{
		"key":       key,
		"size":      len(payload),
		"encrypted": settings.Encrypt,
	}, nil
}

// backupDetail 下载并解析归档。
func (d *Deps) backupDetail(ctx context.Context, key string) (map[string]any, error) {
	settings := d.backupSettings()
	client := service.NewR2Client(settings)
	payload, err := client.DownloadBytes(ctx, key)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(key, ".enc") {
		if settings.Passphrase == "" {
			return nil, service.NewBackupConfigError("当前未配置加密口令，无法查看已加密备份")
		}
		payload, err = service.DecryptOpenSSL(payload, settings.Passphrase)
		if err != nil {
			return nil, err
		}
	}
	detail, err := service.DecodeArchive(payload)
	if err != nil {
		return nil, err
	}
	detail["key"] = key
	detail["name"] = key
	if index := strings.LastIndexByte(key, '/'); index >= 0 {
		detail["name"] = key[index+1:]
	}
	detail["encrypted"] = strings.HasSuffix(key, ".enc")
	return detail, nil
}

// backupDownload 下载（.enc 会自动解密）。
func (d *Deps) backupDownload(ctx context.Context, key string) (map[string]any, error) {
	settings := d.backupSettings()
	client := service.NewR2Client(settings)
	payload, err := client.DownloadBytes(ctx, key)
	if err != nil {
		return nil, err
	}

	name := key
	if index := strings.LastIndexByte(key, '/'); index >= 0 {
		name = key[index+1:]
	}
	if name == "" {
		name = "backup.bin"
	}

	if strings.HasSuffix(key, ".enc") {
		if settings.Passphrase == "" {
			return nil, service.NewBackupConfigError("当前未配置加密口令，无法下载并解密已加密备份")
		}
		payload, err = service.DecryptOpenSSL(payload, settings.Passphrase)
		if err != nil {
			return nil, err
		}
		name = strings.TrimSuffix(name, ".enc")
		if name == "" {
			name = "backup.tar.gz"
		}
	}

	contentType := "application/octet-stream"
	if strings.HasSuffix(name, ".tar.gz") {
		contentType = "application/gzip"
	}
	return map[string]any{
		"key":          key,
		"name":         name,
		"content_type": contentType,
		"payload":      payload,
		"size":         len(payload),
	}, nil
}

// backupState 返回备份运行状态（磁盘内容 + 内存 running）。
func (d *Deps) backupState() map[string]any {
	if d.backupStateStore == nil {
		d.backupStateStore = newBackupStateStore(d.Config.BackupStateFile())
	}
	return d.backupStateStore.Snapshot()
}

func encodeURIPath(value string) string {
	return urlQueryEscape(value)
}
