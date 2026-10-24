package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ContentFilter 在请求进入上游之前做内容检查。
//
// 存在理由：`sensitive_words` 与 `ai_review` 两个配置之前只被保存和回显，
// 请求链路零调用 —— 管理员在设置页填了敏感词，请求照样通过。
// Python 对应实现是 services/content_filter.py:150-264，调用点在
// api/ai.py、api/image_tasks.py 与文本对话入口。
//
// 两级检查：
//  1. 本地敏感词（原始文本子串匹配，无网络）
//  2. AI 审核（启用时调 OpenAI 兼容服务，按 fail_open 决定故障时放行或拒绝）
type ContentFilter struct {
	options ContentFilterOptions
}

// ContentFilterOptions 是依赖注入点（测试不需要真实 config）。
type ContentFilterOptions struct {
	SensitiveWords func() []string
	AIReview       func() map[string]any
	// HTTPClient 可选；nil 时用带 60s 超时的默认客户端（Python 用 timeout=60）。
	HTTPClient *http.Client
}

// ContentFilterError 是内容检查拒绝时返回的错误。
//
// 带状态码是因为两种拒绝语义不同：敏感词/AI 拒绝是 400，
// fail_open=false 时审核服务不可用是 503。
type ContentFilterError struct {
	StatusCode int
	Message    string
}

func (e *ContentFilterError) Error() string { return e.Message }

// 常量逐字对齐 Python（services/content_filter.py:12,22,23）。
const (
	defaultReviewPrompt  = "判断用户请求是否允许。只回答 ALLOW 或 REJECT。"
	maxReviewTextLen     = 100_000
	reviewTruncateMarker = "\n…[truncated]…\n"
)

// base64DataURI 匹配内联图片，送审前替换成 [image]。
var base64DataURI = regexp.MustCompile(`data:[\w/.+;-]+;base64,[A-Za-z0-9+/=]+`)

// NewContentFilter 构造过滤器。
func NewContentFilter(options ContentFilterOptions) *ContentFilter {
	return &ContentFilter{options: options}
}

// Check 检查一段请求文本。nil 表示放行。
func (f *ContentFilter) Check(ctx context.Context, text string) *ContentFilterError {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	// 1. 本地敏感词：原始文本子串匹配，不做任何归一化（Python 同）。
	if f.options.SensitiveWords != nil {
		for _, word := range f.options.SensitiveWords() {
			if word != "" && strings.Contains(text, word) {
				return &ContentFilterError{
					StatusCode: http.StatusBadRequest,
					Message:    "检测到敏感词，拒绝本次任务",
				}
			}
		}
	}

	// 2. AI 审核。
	review := map[string]any{}
	if f.options.AIReview != nil {
		review = f.options.AIReview()
	}
	if !truthy(review["enabled"]) {
		return nil
	}

	baseURL := strings.TrimRight(strings.TrimSpace(stringValue(review["base_url"])), "/")
	apiKey := strings.TrimSpace(stringValue(review["api_key"]))
	model := strings.TrimSpace(stringValue(review["model"]))
	if baseURL == "" || apiKey == "" || model == "" {
		return &ContentFilterError{
			StatusCode: http.StatusBadRequest,
			Message:    "ai review config is incomplete",
		}
	}

	failOpen := resolveFailOpen(review)
	prompt := strings.TrimSpace(stringValue(review["prompt"]))
	if prompt == "" {
		prompt = defaultReviewPrompt
	}

	reviewText, _ := sanitizeForReview(text)
	content := fmt.Sprintf("%s\n\n用户请求:\n%s\n\n只回答 ALLOW 或 REJECT。", prompt, reviewText)

	decision, failure := f.askReview(ctx, baseURL, apiKey, model, content)
	if failure != nil {
		if failOpen {
			return nil
		}
		return &ContentFilterError{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "AI 审核服务暂时不可用，请稍后重试",
		}
	}

	switch {
	case decision == "":
		// 响应形状不符或决策含糊：按 fail_open 处理。
		if failOpen {
			return nil
		}
		return &ContentFilterError{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "AI 审核服务暂时不可用，请稍后重试",
		}
	case isAllowDecision(decision):
		return nil
	case isRejectDecision(decision):
		return &ContentFilterError{
			StatusCode: http.StatusBadRequest,
			Message:    "AI 审核未通过，拒绝本次任务",
		}
	default:
		// 含糊决策（MAYBE / 空）：fail_open 时放行。
		if failOpen {
			return nil
		}
		return &ContentFilterError{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "AI 审核服务暂时不可用，请稍后重试",
		}
	}
}

// askReview 调审核服务，返回小写决策文本；failure 非 nil 表示调用失败。
func (f *ContentFilter) askReview(ctx context.Context, baseURL, apiKey, model, content string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "user", "content": content},
		},
		"temperature": 0,
	})
	if err != nil {
		return "", err
	}

	requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost,
		baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+apiKey)

	client := f.options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("review status %d: %s", response.StatusCode, string(payload))
	}

	return extractReviewDecision(payload), nil
}

// extractReviewDecision 从 OpenAI chat-completion 形状里取决策文本。
//
// 返回空串表示形状不符（调用方按 fail_open 处理）。
func extractReviewDecision(payload []byte) string {
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return ""
	}
	choices, ok := data["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	message, ok := first["message"].(map[string]any)
	if !ok {
		return ""
	}
	content, ok := message["content"]
	if !ok || content == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(stringValue(content)))
}

// isAllowDecision / isRejectDecision 逐字对齐 Python 的前缀匹配。
func isAllowDecision(decision string) bool {
	for _, prefix := range []string{"allow", "pass", "true", "yes", "通过", "允许", "安全"} {
		if strings.HasPrefix(decision, prefix) {
			return true
		}
	}
	return false
}

func isRejectDecision(decision string) bool {
	for _, prefix := range []string{"reject", "deny", "block", "false", "no", "拒绝", "不允许", "违规", "禁止"} {
		if strings.HasPrefix(decision, prefix) {
			return true
		}
	}
	return false
}

// resolveFailOpen 解析 fail_open，默认 true（Python 同）。
func resolveFailOpen(review map[string]any) bool {
	value, exists := review["fail_open"]
	if !exists || value == nil {
		return true
	}
	return truthy(value)
}

// ReviewSanitizeStats 记录送审文本的清洗情况。
type ReviewSanitizeStats struct {
	Base64BlocksStripped int
	TruncatedChars       int
}

// sanitizeForReview 剥离 base64 内联图片并截断到审核服务的上下文上限。
//
// 为什么要剥离：纯文本审核模型看不懂图片字节，而一张内联图片轻易
// 超出上下文限制 —— 不剥离会让审核请求直接失败。
func sanitizeForReview(text string) (string, ReviewSanitizeStats) {
	stats := ReviewSanitizeStats{}

	matches := base64DataURI.FindAllString(text, -1)
	stats.Base64BlocksStripped = len(matches)
	sanitized := base64DataURI.ReplaceAllString(text, "[image]")

	if len(sanitized) > maxReviewTextLen {
		// 预留标记空间，保证结果不超上限（Python 同）。
		half := (maxReviewTextLen - len(reviewTruncateMarker)) / 2
		if half < 0 {
			half = 0
		}
		stats.TruncatedChars = len(sanitized) - 2*half
		if half > 0 && len(sanitized) > half {
			sanitized = sanitized[:half] + reviewTruncateMarker + sanitized[len(sanitized)-half:]
		}
	}
	return sanitized, stats
}
