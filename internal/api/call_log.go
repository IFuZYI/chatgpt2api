package api

import (
	"strings"
	"time"
)

// loggedCall 记录一次 /v1 调用日志（LogTypeCall）。
//
// 前端日志页默认打开就是 type=call，所以没有 call 日志时首屏恒为空，
// 用户会以为日志功能坏了。字段与 Python log_service.LoggedCall 对齐：
//
//	summary       "生图 成功" / "文本对话 失败"
//	key_name      调用方密钥名
//	status        "success" | "failed"
//	duration_ms   耗时
//	endpoint      "/v1/images/generations"
//	model         模型名
//	account_email 实际使用的账号
//	urls          产出图片 URL
//	error         失败原因
type callLog struct {
	deps     *Deps
	summary  string
	endpoint string
	model    string
	keyName  string
	started  time.Time
}

// beginCall 开始计时。
func (d *Deps) beginCall(summary, endpoint, model, keyName string) *callLog {
	return &callLog{
		deps:     d,
		summary:  summary,
		endpoint: endpoint,
		model:    model,
		keyName:  keyName,
		started:  time.Now(),
	}
}

// finish 写入日志。urls / accountEmail 为空时省略对应字段（与 Python 一致）。
func (c *callLog) finish(err error, accountEmail string, urls []string) {
	if c == nil || c.deps == nil || c.deps.Logs == nil {
		return
	}

	detail := map[string]any{
		"endpoint":    c.endpoint,
		"duration_ms": time.Since(c.started).Milliseconds(),
	}
	if c.model != "" {
		detail["model"] = c.model
	}
	if name := strings.TrimSpace(c.keyName); name != "" {
		detail["key_name"] = name
	}

	suffix := " 成功"
	if err != nil {
		detail["status"] = "failed"
		detail["error"] = err.Error()
		suffix = " 失败"
	} else {
		detail["status"] = "success"
	}

	if email := strings.TrimSpace(accountEmail); email != "" {
		detail["account_email"] = email
	}
	if len(urls) > 0 {
		detail["urls"] = urls
	}

	c.deps.Logs.Add("call", c.summary+suffix, detail)
}
