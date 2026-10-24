package api

import (
	"context"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

// checkContent 对一段请求文本做内容检查。
//
// 未注入过滤器时直接放行（测试与未接线场景）。
func (d *Deps) checkContent(ctx context.Context, text string) error {
	if d.ContentFilter == nil {
		return nil
	}
	if err := d.ContentFilter.Check(ctx, text); err != nil {
		return err
	}
	return nil
}

// checkContentMessages 从消息列表里抽出最后一条 user 文本再检查。
//
// 只查最后一条 user 消息：历史里的旧内容在上一轮已经查过，
// 重复检查会让多轮对话在第 3、4 轮突然被拒。
func (d *Deps) checkContentMessages(ctx context.Context, messages []map[string]any) error {
	if d.ContentFilter == nil {
		return nil
	}
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if strings.TrimSpace(stringValueOf(message["role"])) != "user" {
			continue
		}
		return d.checkContent(ctx, messageTextOf(message["content"]))
	}
	return nil
}

// messageTextOf 把 content 归一成纯文本（字符串或 content parts 数组）。
func messageTextOf(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	parts, ok := content.([]any)
	if !ok {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		object, ok := part.(map[string]any)
		if !ok {
			continue
		}
		switch stringValueOf(object["type"]) {
		case "text", "input_text":
			builder.WriteString(stringValueOf(object["text"]))
		}
	}
	return builder.String()
}

// contentFilterError 把 service 层的错误转成 HTTP 信封。
func contentFilterError(err error) *HTTPError {
	if filterErr, ok := err.(*service.ContentFilterError); ok {
		return Errorf(filterErr.StatusCode, filterErr.Message)
	}
	return Errorf(500, err.Error())
}

// guardPrompt 对 prompt 做内容检查，返回非 nil 表示已写好响应、调用方应直接返回。
//
// 收口成一个函数的原因：图片链有 4 个 prompt 入口（同步 generations/edits、
// 异步任务 generations/edits），逐个手写检查漏一个就是一条绕过路径。
func (d *Deps) guardPrompt(c *gin.Context, prompt string) bool {
	if err := d.checkContent(c.Request.Context(), prompt); err != nil {
		writeErr(c, contentFilterError(err))
		return true
	}
	return false
}
