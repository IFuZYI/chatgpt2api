package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// HealthPayload 复刻 /health?format=json 的结构，字段名是契约。
func (d *Deps) healthPayload() map[string]any {
	stats := d.Accts.Stats()
	active := intValueOf(stats["active"])
	healthy := active > 0

	status := "degraded"
	if healthy {
		status = "ok"
	}
	return map[string]any{
		"status":  status,
		"healthy": healthy,
		"version": d.Config.AppVersion(),
		"storage": map[string]any{
			"backend": redactStoragePaths(d.Storage.BackendInfo()),
			"health":  redactStoragePaths(d.Storage.HealthCheck()),
		},
		"proxy_runtime": d.proxyRuntimeStatus(),
		"accounts":      stats,
	}
}

func intValueOf(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func stringValueOf(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// RegisterSystemRoutes 注册 /version、/health、/auth/login。
func (d *Deps) RegisterSystemRoutes(r *gin.Engine) {
	r.GET("/version", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"version": d.Config.AppVersion()})
	})

	r.POST("/auth/login", func(c *gin.Context) {
		identity, err := d.RequireIdentity(c.GetHeader("Authorization"))
		if err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":         true,
			"version":    d.Config.AppVersion(),
			"role":       identity.Role,
			"subject_id": identity.ID,
			"name":       identity.Name,
		})
	})

	r.GET("/api/logs", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		items := d.Logs.List(
			strings.TrimSpace(c.Query("type")),
			strings.TrimSpace(c.Query("start_date")),
			strings.TrimSpace(c.Query("end_date")),
			200,
		)
		c.JSON(http.StatusOK, gin.H{"items": items})
	})

	r.POST("/api/logs/delete", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}
		body, err := decodeJSONBody(c)
		if err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, d.Logs.Delete(stringList(body["ids"])))
	})

	r.GET("/health", func(c *gin.Context) {
		payload := d.healthPayload()
		if strings.TrimSpace(c.Query("format")) == "json" {
			c.JSON(http.StatusOK, payload)
			return
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, renderHealthHTML(payload))
	})
}

// renderHealthHTML 逐字复刻 Python 里的内联模板，包括 CSS 与自动刷新。
func renderHealthHTML(payload map[string]any) string {
	stats, _ := payload["accounts"].(map[string]any)
	healthy, _ := payload["healthy"].(bool)
	version := stringValueOf(payload["version"])

	statusDot := "status-degraded"
	statusText := "异常"
	statusClass := "yellow"
	if healthy {
		statusDot = "status-ok"
		statusText = "正常"
		statusClass = "green"
	}

	byType, _ := stats["by_type"].(map[string]any)
	typeKeys := make([]string, 0, len(byType))
	for key := range byType {
		typeKeys = append(typeKeys, key)
	}
	sort.Strings(typeKeys)
	var rows strings.Builder
	for _, key := range typeKeys {
		fmt.Fprintf(&rows, "<tr><td>%s</td><td>%d</td></tr>", htmlEscape(key), intValueOf(byType[key]))
	}

	return `<!DOCTYPE html>
<html lang="zh">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>号池健康监控 - chatgpt2api</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:system-ui,-apple-system,sans-serif;background:#0f1117;color:#e2e8f0;min-height:100vh}
.header{background:#1a1d27;border-bottom:1px solid #2a2d3a;padding:16px 24px;display:flex;justify-content:space-between;align-items:center}
.header h1{font-size:20px}
.status-dot{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:8px}
.status-ok{background:#22c55e;box-shadow:0 0 8px #22c55e88}
.status-degraded{background:#f59e0b;box-shadow:0 0 8px #f59e0b88}
.container{max-width:960px;margin:0 auto;padding:24px}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:12px;margin-bottom:24px}
.card{background:#1a1d27;border:1px solid #2a2d3a;border-radius:10px;padding:16px}
.card .value{font-size:28px;font-weight:700;margin:4px 0}
.card .label{font-size:13px;color:#94a3b8}
.green{color:#22c55e}.yellow{color:#f59e0b}.red{color:#ef4444}.blue{color:#6c63ff}
table{width:100%;border-collapse:collapse;background:#1a1d27;border:1px solid #2a2d3a;border-radius:10px;overflow:hidden}
th{background:#242836;font-weight:600;text-align:left;padding:10px 12px;font-size:12px;color:#94a3b8;text-transform:uppercase}
td{padding:8px 12px;border-top:1px solid #2a2d3a;font-size:14px}tr:hover td{background:rgba(108,99,255,.05)}
.api-url{font-family:monospace;font-size:12px;color:#6c63ff}
.refresh{font-size:12px;color:#64748b;text-align:center;margin-top:24px}
</style>
<meta http-equiv="refresh" content="30">
</head>
<body>
<div class="header">
<h1><span class="status-dot ` + statusDot + `"></span>号池健康监控</h1>
<div style="font-size:13px;color:#94a3b8">v` + htmlEscape(version) + ` · 30s 自动刷新</div>
</div>
<div class="container">
<div class="cards">
<div class="card"><div class="label">号池状态</div><div class="value ` + statusClass + `">` + statusText + `</div></div>
<div class="card"><div class="label">当前账号</div><div class="value blue">` + itoa(intValueOf(stats["total"])) + `</div></div>
<div class="card"><div class="label">累计入库</div><div class="value">` + itoa(intValueOf(stats["cumulative_total"])) + `</div></div>
<div class="card"><div class="label">可用账号</div><div class="value green">` + itoa(intValueOf(stats["active"])) + `</div></div>
<div class="card"><div class="label">剩余额度</div><div class="value">` + itoa(intValueOf(stats["total_quota"])) + `</div></div>
<div class="card"><div class="label">限流</div><div class="value yellow">` + itoa(intValueOf(stats["limited"])) + `</div></div>
<div class="card"><div class="label">异常</div><div class="value red">` + itoa(intValueOf(stats["abnormal"])) + `</div></div>
<div class="card"><div class="label">禁用</div><div class="value">` + itoa(intValueOf(stats["disabled"])) + `</div></div>
<div class="card"><div class="label">成功/失败</div><div class="value">` + itoa(intValueOf(stats["total_success"])) +
		`<span style="font-size:18px;color:#94a3b8">/</span><span class="red">` +
		itoa(intValueOf(stats["total_fail"])) + `</span></div></div>
</div>
<h2 style="margin-bottom:12px;font-size:16px">账号类型分布</h2>
<table>
<tr><th>类型</th><th>数量</th></tr>
` + rows.String() + `
</table>
<div class="refresh">JSON: <span class="api-url">/health?format=json</span></div>
</div></body></html>`
}

func itoa(value int) string {
	return fmt.Sprintf("%d", value)
}

func htmlEscape(text string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return replacer.Replace(text)
}

// proxyRuntimeStatus 直接复用 ProxySettings.RuntimeStatus，
// 键名与 Python 的 get_runtime_status 完全一致（/health 与设置页都读它）。
func (d *Deps) proxyRuntimeStatus() map[string]any {
	if d.Proxy == nil {
		return map[string]any{
			"enabled":                false,
			"egress_mode":            "direct",
			"proxy_source":           "direct",
			"has_proxy":              false,
			"clearance_enabled":      false,
			"clearance_mode":         "none",
			"has_clearance_bundle":   false,
			"cached_clearance_hosts": []any{},
		}
	}
	return d.Proxy.RuntimeStatus()
}

// 入站请求体上限。
//
// 为什么必须有：decodeJSONBody 原来用 c.GetRawData() 无限制读入，而
// /v1/images/edits 会把 base64 图片解出来做 mask 合成 —— 解码放大实测
// 约 325 倍（1.5 MB 请求体 → RSS +459 MB）。一次 52 秒的请求就能把
// 单实例推到 461 MB；几个并发即 OOM。
//
// 取值依据：
//
//   - 控制面 8 MiB。不能取更小：`POST /api/accounts` 导入的是整个号池的
//     JSON，实测 260 个账号的导出文件已有 880 KB（约 3.5 KB/账号）。
//     1 MiB 只能装 302 个账号 —— 一个 300 号以上的池子导出后**无法再导入**。
//     8 MiB 可容纳约 2400 个账号，对单实例号池足够。
//
//   - 图片端点 16 MiB。base64 有 4/3 膨胀，16 MiB 请求体 ≈ 12 MB 原始图。
//     真正的边界是 maxImagePixels（解码后像素数），不是这里的字节数；
//     字节上限只负责挡住「先把我撑爆再谈像素」这一层。
const (
	maxJSONBodyBytes  = 8 << 20  // 8 MiB
	maxImageBodyBytes = 16 << 20 // 16 MiB（base64 图片）
)

// isImageBodyPath 报告该端点是否接受 base64 图片。
//
// 漏一个端点的后果不是「限额松一点」而是「合法上传被 413」——
// /v1/ppt/generations 与 /v1/psd/generations 都收 base64_images
// （PSD 还**要求**至少一张参考图），漏在列表外会让 >~750 KB 的参考图被拒。
func isImageBodyPath(path string) bool {
	switch path {
	case "/v1/images/edits", "/v1/images/generations",
		"/v1/chat/completions", "/v1/responses",
		"/v1/ppt/generations", "/v1/psd/generations",
		"/api/image-tasks/edits", "/api/image-tasks/generations":
		return true
	}
	return false
}

// bodyLimitFor 返回该请求的 body 上限。
func bodyLimitFor(c *gin.Context) int64 {
	if isImageBodyPath(c.FullPath()) {
		return maxImageBodyBytes
	}
	return maxJSONBodyBytes
}

// decodeJSONBody 读原始 body 成 map，保持 Python 的宽松解析（不校验多余字段）。
//
// 读之前先用 http.MaxBytesReader 套上上限：超限时 GetRawData 返回错误，
// 这里映射成 413。必须在**读之前**设，读完再检查等于已经分配过内存了。
func decodeJSONBody(c *gin.Context) (map[string]any, *HTTPError) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, bodyLimitFor(c))
	payload, err := c.GetRawData()
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, Errorf(http.StatusRequestEntityTooLarge,
				"request body too large: limit is "+strconv.FormatInt(bodyLimitFor(c), 10)+" bytes")
		}
		return nil, Errorf(http.StatusBadRequest, "invalid request body")
	}
	if len(strings.TrimSpace(string(payload))) == 0 {
		return map[string]any{}, nil
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, Errorf(http.StatusBadRequest, "invalid JSON body")
	}
	if body == nil {
		return map[string]any{}, nil
	}
	return body, nil
}

// storagePathFields 是存储信息里会带绝对路径的字段。
//
// /health 是**未鉴权**端点（Python 版契约如此，监控系统要能直接探活）。
// 原始实现把 /home/<user>/chatgpt2api/data/accounts.json 这类绝对路径
// 直接返回给任何访问者 —— 攻击者据此可定位部署目录、判断是否容器、
// 推断宿主机用户名。健康检查不需要路径，只需要「文件在不在」。
var storagePathFields = []string{"file_path", "auth_keys_file_path"}

// redactStoragePaths 把绝对路径替换成只保留文件名（并保留 *_exists 布尔）。
//
// 保留文件名（accounts.json / auth_keys.json）是为了排障时仍能看出
// 「后端在读写哪个文件」，去掉的是目录部分。
func redactStoragePaths(info map[string]any) map[string]any {
	if info == nil {
		return info
	}
	out := make(map[string]any, len(info))
	for key, value := range info {
		out[key] = value
	}
	for _, field := range storagePathFields {
		raw, ok := out[field].(string)
		if !ok || raw == "" {
			continue
		}
		out[field] = filepath.Base(raw)
	}
	return out
}
