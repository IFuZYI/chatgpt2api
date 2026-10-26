package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"

	"github.com/IFuZYI/chatgpt2api/internal/protocol"
)

// 让 *Client 满足 protocol.ImageUpstream 接口。
var _ protocol.ImageUpstream = (*Client)(nil)

// GetConversation 拉取完整对话文档（轮询用）。
func (c *Client) GetConversation(ctx context.Context, conversationID string) (map[string]any, error) {
	path := "/backend-api/conversation/" + conversationID
	body, status, err := c.doGet(ctx, path, 60*time.Second)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, NewStatusError(status, retryAfterFromBody(body), fmt.Sprintf("%s failed: status=%d", path, status))
	}
	var conversation map[string]any
	if err := json.Unmarshal(body, &conversation); err != nil {
		return nil, fmt.Errorf("%s: invalid json: %w", path, err)
	}
	return conversation, nil
}

// QueryTasks 查 /backend-api/tasks，按 conversation_id 过滤。
func (c *Client) QueryTasks(ctx context.Context, conversationID string) ([]map[string]any, error) {
	path := "/backend-api/tasks"
	body, status, err := c.doGet(ctx, path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("%s failed: status=%d", path, status)
	}
	var wrapper map[string]any
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, err
	}
	tasks, _ := wrapper["tasks"].([]any)
	out := make([]map[string]any, 0, len(tasks))
	for _, item := range tasks {
		task, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if conversationID == "" {
			out = append(out, task)
			continue
		}
		// 按 conversation_id 或 original_conversation_id 匹配（Python 两个都认）。
		if stringOfValue(task["conversation_id"]) == conversationID ||
			stringOfValue(task["original_conversation_id"]) == conversationID {
			out = append(out, task)
		}
	}
	return out, nil
}

// ResolveImageURLs 把 file_id / sediment_id 解析成可下载 URL。
//
// 跳过 file_upload* 这类非图片 ID（与 Python 的 skip_patterns 一致）。
func (c *Client) ResolveImageURLs(
	ctx context.Context,
	conversationID string,
	fileIDs, sedimentIDs []string,
) ([]string, error) {
	urls := make([]string, 0, len(fileIDs)+len(sedimentIDs))
	seen := map[string]bool{}

	appendURL := func(url string) {
		if url == "" || seen[url] {
			return
		}
		seen[url] = true
		urls = append(urls, url)
	}

	for _, fileID := range fileIDs {
		if strings.HasPrefix(fileID, "file_upload") {
			continue
		}
		url, err := c.fileDownloadURL(ctx, fileID)
		if err != nil {
			continue
		}
		appendURL(url)
	}

	if conversationID == "" || len(sedimentIDs) == 0 {
		return urls, nil
	}
	for _, sedimentID := range sedimentIDs {
		url, err := c.attachmentDownloadURL(ctx, conversationID, sedimentID)
		if err != nil {
			continue
		}
		appendURL(url)
	}
	return urls, nil
}

func (c *Client) fileDownloadURL(ctx context.Context, fileID string) (string, error) {
	path := "/backend-api/files/" + fileID + "/download"
	body, status, err := c.doGet(ctx, path, 60*time.Second)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("%s failed: status=%d", path, status)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if url := stringOfValue(payload["download_url"]); url != "" {
		return url, nil
	}
	return stringOfValue(payload["url"]), nil
}

func (c *Client) attachmentDownloadURL(ctx context.Context, conversationID, attachmentID string) (string, error) {
	path := "/backend-api/conversation/" + conversationID + "/attachment/" + attachmentID + "/download"
	body, status, err := c.doGet(ctx, path, 60*time.Second)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("%s failed: status=%d", path, status)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if url := stringOfValue(payload["download_url"]); url != "" {
		return url, nil
	}
	return stringOfValue(payload["url"]), nil
}

// DownloadImage 下载图片字节。去重在 protocol 层做，这里只负责取。
func (c *Client) DownloadImage(ctx context.Context, url string) ([]byte, error) {
	// 必须自带 deadline：上游可能在 body 中途停住（发完 header 不再发数据），
	// io.ReadAll 会永久阻塞。这个调用发生在 emitResult 内、仍在账号槽位
	// 持有期，一个卡住的下载 = 一个永久泄漏的槽位。
	ctx, cancel := applyRequestTimeout(ctx, 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// 图片 URL 是 chatgpt.com/backend-api/estuary/content?...&sig=... 形式，
	// 虽然带签名，仍然要求登录态：不带 Authorization 会稳定 404（实测）。
	// Python 版用同一个 session（session.headers 里就有 Authorization）所以能下载。
	headers := map[string]string{
		"Accept":          "image/*,*/*",
		"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
		"Referer":         BaseURL + "/",
	}
	if strings.TrimSpace(c.accessToken) != "" {
		headers["Authorization"] = "Bearer " + c.accessToken
	}
	req.Header = c.headers("/backend-api/estuary/content", headers)
	// 下载图片不需要 target-path/route，去掉避免上游把它当 API 调用处理。
	delete(req.Header, "x-openai-target-path")
	delete(req.Header, "x-openai-target-route")
	delete(req.Header, "X-OpenAI-Target-Path")
	delete(req.Header, "X-OpenAI-Target-Route")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("image download failed: status=%d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
}

// DeleteConversation 把对话标记为不可见（PATCH is_visible=false）。
func (c *Client) DeleteConversation(ctx context.Context, conversationID string) error {
	path := "/backend-api/conversation/" + conversationID
	payload, err := json.Marshal(map[string]any{"is_visible": false})
	if err != nil {
		return err
	}
	extra := map[string]string{
		"Accept":       "*/*",
		"Content-Type": "application/json",
		"Referer":      BaseURL + "/c/" + conversationID,
		// 注意：这里上游要求 target-route 带字面量 {conversation_id}，不是真实 id。
		"X-OpenAI-Target-Route": "/backend-api/conversation/{conversation_id}",
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, BaseURL+path, newBytesReader(payload))
	if err != nil {
		return err
	}
	req.Header = c.headers(path, extra)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s failed: status=%d", path, resp.StatusCode)
	}
	return nil
}

// FindConversationByPrompt 复刻 find_conversation_by_prompt：
// 在最近对话里按标题关键词与时间窗匹配，匹配不上则返回最新的一条。
func (c *Client) FindConversationByPrompt(ctx context.Context, prompt string, since time.Time) (string, error) {
	items, err := c.listRecentConversations(ctx, 10)
	if err != nil || len(items) == 0 {
		return "", err
	}

	startedAt := float64(since.Unix())
	promptLower := strings.ToLower(strings.TrimSpace(prompt))
	promptWords := wordSet(promptLower)

	bestMatch := ""
	bestScore := 0.0
	for _, item := range items {
		conversationID := conversationIDOf(item)
		if conversationID == "" {
			continue
		}
		updatedAt := numberValue(item["update_time"])
		if updatedAt == 0 {
			updatedAt = numberValue(item["updated_at"])
		}
		// 时间窗：早 30s 到晚 600s 之间才算候选。
		if updatedAt != 0 && startedAt != 0 && (updatedAt < startedAt-30 || updatedAt > startedAt+600) {
			continue
		}
		title := strings.ToLower(stringOfValue(item["title"]))
		score := 0.0
		if promptLower != "" && title != "" {
			titleWords := wordSet(title)
			common := 0
			for word := range promptWords {
				if titleWords[word] {
					common++
				}
			}
			if common > 0 {
				score = float64(common) / float64(maxIntValue(len(promptWords), 1))
			}
		}
		// 图生图的标题通常以 "image" 开头，给一点加权。
		if strings.HasPrefix(title, "image") {
			score += 0.3
		}
		if score > bestScore {
			bestScore = score
			bestMatch = conversationID
		}
	}
	if bestMatch != "" && bestScore > 0.1 {
		return bestMatch, nil
	}

	// 没有标题匹配：退回时间最近的一条。
	for _, item := range items {
		conversationID := conversationIDOf(item)
		updatedAt := numberValue(item["update_time"])
		if updatedAt == 0 {
			updatedAt = numberValue(item["updated_at"])
		}
		if conversationID != "" && updatedAt != 0 && startedAt != 0 && updatedAt >= startedAt-30 {
			return conversationID, nil
		}
	}
	return "", nil
}

func (c *Client) listRecentConversations(ctx context.Context, limit int) ([]map[string]any, error) {
	path := fmt.Sprintf("/backend-api/conversations?offset=0&limit=%d&order=updated&conversation_filter=all", limit)
	body, status, err := c.doGet(ctx, path, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("%s failed: status=%d", path, status)
	}
	var wrapper map[string]any
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, err
	}
	rawItems, _ := wrapper["items"].([]any)
	if rawItems == nil {
		rawItems, _ = wrapper["conversations"].([]any)
	}
	out := make([]map[string]any, 0, len(rawItems))
	for _, item := range rawItems {
		if entry, ok := item.(map[string]any); ok {
			out = append(out, entry)
		}
	}
	return out, nil
}

func conversationIDOf(item map[string]any) string {
	if id := stringOfValue(item["id"]); id != "" {
		return id
	}
	return stringOfValue(item["conversation_id"])
}

func wordSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, word := range strings.Fields(text) {
		if word != "" {
			out[word] = true
		}
	}
	return out
}

func maxIntValue(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// doGet 是带指纹的 GET。
func (c *Client) doGet(ctx context.Context, path string, timeout time.Duration) ([]byte, int, error) {
	// timeout 必须真正生效：客户端级超时已设为 0（见 NewClient），
	// 若这里也不加 deadline，上游挂住时这个请求永不返回 ——
	// generateWithRetry 的 defer release() 不执行，账号槽位永久 inflight。
	// 与 doFull / doWithRawHeaders 的写法保持一致。
	ctx, cancel := applyRequestTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header = c.headers(path, map[string]string{"Accept": "application/json"})

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// NewStatusError 构造带状态码的错误，供 protocol 的退避逻辑识别。
func NewStatusError(status int, retryAfter *int, message string) error {
	return protocol.NewHTTPStatusError(status, retryAfter, message)
}

// retryAfterFromBody 从响应体里尽力提取 retry_after（上游有时放在 body 里）。
func retryAfterFromBody(body []byte) *int {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	value, ok := payload["retry_after"].(float64)
	if !ok {
		return nil
	}
	seconds := int(value)
	return &seconds
}

// FetchAccountInfo 拉取账号信息（邮箱、套餐、额度、恢复时间）。
//
// FetchAccountInfo 拉取账号信息。
//
// 注意：这里是**串行**三次请求（me → conversation/init → accounts/check），
// 不是并发。原注释写「并发查」与实现不符 —— 串行意味着单账号耗时是三者之和，
// 批量刷新 N 个账号的时间线性叠加（这也是上传接口默认不刷新、
// 把刷新交给后台 watcher 的原因之一）。
//
// 按 image_gen 的剩余额度与恢复时间组装返回值。
func (c *Client) FetchAccountInfo(ctx context.Context) (map[string]any, error) {
	if c.accessToken == "" {
		return nil, fmt.Errorf("access_token is required")
	}

	meBody, meStatus, err := c.doGet(ctx, "/backend-api/me", 30*time.Second)
	if err != nil {
		return nil, err
	}
	if meStatus >= 400 {
		return nil, NewStatusError(meStatus, nil, fmt.Sprintf("/backend-api/me failed: status=%d", meStatus))
	}
	var me map[string]any
	_ = json.Unmarshal(meBody, &me)

	// conversation/init 是 POST 且必须带 body。
	// 用 GET 会稳定返回 400 {"detail":"Invalid conversation init"} —— 实测踩到。
	initRequestBody, marshalErr := json.Marshal(map[string]any{
		"gizmo_id":                nil,
		"requested_default_model": nil,
		"conversation_id":         nil,
		"timezone_offset_min":     -480,
	})
	if marshalErr != nil {
		return nil, marshalErr
	}
	initBody, initStatus, err := c.doPostJSON(ctx, "/backend-api/conversation/init", initRequestBody, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if initStatus >= 400 {
		return nil, NewStatusError(initStatus, nil, fmt.Sprintf("/backend-api/conversation/init failed: status=%d", initStatus))
	}
	var initPayload map[string]any
	_ = json.Unmarshal(initBody, &initPayload)

	planType := "free"
	if accountsBody, accountsStatus, accountsErr := c.doGet(ctx,
		"/backend-api/accounts/check/v4-2023-04-27", 30*time.Second); accountsErr == nil && accountsStatus < 400 {
		var accountsPayload map[string]any
		if json.Unmarshal(accountsBody, &accountsPayload) == nil {
			if plan := findPlanType(accountsPayload); plan != "" {
				planType = plan
			}
		}
	}

	limitsProgress, _ := initPayload["limits_progress"].([]any)
	if limitsProgress == nil {
		limitsProgress = []any{}
	}
	quota, restoreAt := quotaFromLimits(limitsProgress)

	status := "正常"
	if quota == 0 {
		status = "限流"
	}

	email := stringOfValue(me["email"])
	userID := stringOfValue(me["id"])
	return map[string]any{
		"email":              nilIfEmptyValue(email),
		"user_id":            nilIfEmptyValue(userID),
		"type":               planType,
		"quota":              quota,
		"limits_progress":    limitsProgress,
		"default_model_slug": nilIfEmptyValue(stringOfValue(initPayload["default_model_slug"])),
		"restore_at":         nilIfEmptyValue(restoreAt),
		"status":             status,
	}, nil
}

// findPlanType 在 accounts/check 的嵌套结构里找套餐类型。
func findPlanType(payload map[string]any) string {
	if value := stringOfValue(payload["plan_type"]); value != "" {
		return value
	}
	for _, key := range []string{"account_plan", "account_type", "subscription_type", "type"} {
		if value := stringOfValue(payload[key]); value != "" {
			return value
		}
	}
	for _, value := range payload {
		switch typed := value.(type) {
		case map[string]any:
			if found := findPlanType(typed); found != "" {
				return found
			}
		case []any:
			for _, item := range typed {
				if entry, ok := item.(map[string]any); ok {
					if found := findPlanType(entry); found != "" {
						return found
					}
				}
			}
		}
	}
	return ""
}

// quotaFromLimits 从 limits_progress 里取 image_gen 的剩余额度与恢复时间。
func quotaFromLimits(limits []any) (int, string) {
	quota := 0
	restoreAt := ""
	for _, item := range limits {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if stringOfValue(entry["feature_name"]) != "image_gen" {
			continue
		}
		quota = int(numberValue(entry["remaining"]))
		if reset := stringOfValue(entry["reset_after"]); reset != "" {
			restoreAt = reset
		}
	}
	return quota, restoreAt
}

func nilIfEmptyValue(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// doPostJSON 发一个带指纹的 JSON POST。
func (c *Client) doPostJSON(ctx context.Context, path string, payload []byte, timeout time.Duration) ([]byte, int, error) {
	// 同 doGet：timeout 必须转成 ctx deadline。
	ctx, cancel := applyRequestTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+path, newBytesReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header = c.headers(path, map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
	})

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// ListModels 复刻 list_models：返回上游模型列表（OpenAI /v1/models 形状）。
//
// 有 token 走 /backend-api/models，匿名走 /backend-anon/models。
// 注意这个端点**只返回文本模型**，图片模型（gpt-image-2 系列）不在其中，
// 所以图片模型必须写死在 protocol.baseImageModels 里（见那里的注释）。
func (c *Client) ListModels(ctx context.Context) ([]map[string]any, error) {
	path := "/backend-anon/models?iim=false&is_gizmo=false"
	route := "/backend-anon/models"
	if strings.TrimSpace(c.accessToken) != "" {
		path = "/backend-api/models?history_and_training_disabled=false"
		route = "/backend-api/models"
	}

	body, status, err := c.doGet(ctx, path, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, NewStatusError(status, nil, fmt.Sprintf("%s failed: status=%d", route, status))
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%s: invalid json: %w", route, err)
	}
	models, _ := payload["models"].([]any)

	data := make([]map[string]any, 0, len(models))
	seen := map[string]bool{}
	for _, item := range models {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		slug := strings.TrimSpace(stringOfValue(entry["slug"]))
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true

		ownedBy := strings.TrimSpace(stringOfValue(entry["owned_by"]))
		if ownedBy == "" {
			ownedBy = "chatgpt"
		}
		data = append(data, map[string]any{
			"id":         slug,
			"object":     "model",
			"created":    int(numberValue(entry["created"])),
			"owned_by":   ownedBy,
			"permission": []any{},
			"root":       slug,
			"parent":     nil,
		})
	}
	sort.SliceStable(data, func(i, j int) bool {
		return stringOfValue(data[i]["id"]) < stringOfValue(data[j]["id"])
	})
	return data, nil
}

// StatusCodeOf 取出错误里的上游 HTTP 状态码（没有则 0）。
//
// 导出给 service 层用：刷新链路需要判断 401/403 是「token 失效」
// 还是「限流/网络抖动」，后者不该触发删号。
func StatusCodeOf(err error) int {
	return protocol.StatusCodeOf(err)
}

// applyRequestTimeout 把 timeout 转成 ctx deadline；timeout<=0 时不加。
//
// 为什么单独抽出来：客户端级超时被设为 0（见 NewClient），所有超时都靠
// 每次请求的 ctx。抽成纯函数后可以用单元测试精确断言「timeout 真的
// 变成了 deadline」——tls_client.HttpClient 接口无法注入 transport，
// 直接测 doGet 拿不到这个证据。
//
// 若父 ctx 已有更早的 deadline，context.WithTimeout 会保留它（更早者胜）。
func applyRequestTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return ctx, func() {}
}
