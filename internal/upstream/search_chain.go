package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"

	"github.com/IFuZYI/chatgpt2api/internal/protocol"
)

// 搜索链路（复刻 services/openai_backend_api.py 的 search 系列方法，:1824-1959）。
//
// 与图片链路同源：都是 prepare 换 conduit_token → 打 /f/conversation 建会话。
// 差别只在请求体（system_hints=["search"]、force_use_search）与结果来源
// （不是 SSE 里的图片引用，而是轮询会话文档里的 assistant 消息）。
//
// 所以这里复用 imageHeaders 的指纹头、chatRequirements 的 sentinel 握手、
// readSSEPayloads 的 SSE 解析，不重复实现协议细节。

const (
	// searchTimeout 复刻 SEARCH_TIMEOUT_SECS（300s）。
	searchTimeout = 300 * time.Second
	// searchPollInterval 复刻 SEARCH_POLL_INTERVAL_SECS（3s）。
	searchPollInterval = 3 * time.Second
)

// searchToleratedStatuses 复刻 _wait_search_result 里容忍的状态码集合。
//
// 会话刚建好时上游会短暂返回这些状态（尤其是 404/409 —— 文档还没落库），
// 直接冒泡会让搜索随机失败；401/403 这类真错误必须立刻抛出。
var searchToleratedStatuses = map[int]bool{
	404: true,
	409: true,
	423: true,
	429: true,
	500: true,
	502: true,
	503: true,
	504: true,
}

// 让 *Client 满足 protocol.SearchUpstream 接口。
var _ protocol.SearchUpstream = (*Client)(nil)

// searchOptions 让测试压缩超时与轮询间隔；生产值见上面的常量。
type searchOptions struct {
	timeout      time.Duration
	pollInterval time.Duration
}

// Search 实现 protocol.SearchUpstream。
func (c *Client) Search(ctx context.Context, prompt, model string) (map[string]any, error) {
	return c.search(ctx, prompt, model, searchOptions{
		timeout:      searchTimeout,
		pollInterval: searchPollInterval,
	})
}

// search 复刻 search（:1824-1831）：prepare → bootstrap → conversation → poll。
func (c *Client) search(
	ctx context.Context,
	prompt, model string,
	options searchOptions,
) (map[string]any, error) {
	if strings.TrimSpace(c.accessToken) == "" {
		return nil, fmt.Errorf("access_token is required for search")
	}
	if strings.TrimSpace(model) == "" {
		model = protocol.SearchModel
	}

	conduitToken, err := c.prepareSearchConversation(ctx, prompt, model)
	if err != nil {
		return nil, err
	}
	// 顺序与 Python 一致：先 prepare 再 bootstrap。bootstrap 只补充 PoW 脚本
	// 引用，调换不会改变结果，但跟着上游源码走少一个「为什么不一样」的问题。
	if err := c.Bootstrap(ctx); err != nil {
		return nil, err
	}

	conversationID, err := c.runSearchConversation(ctx, prompt, conduitToken, model)
	if err != nil {
		return nil, err
	}

	result, err := c.waitSearchResult(ctx, conversationID, options)
	if err != nil {
		return nil, err
	}
	return result.Map(), nil
}

// prepareSearchConversation 复刻 _prepare_search_conversation（:1833-1865）。
//
// 注意：这一步不带 sentinel token（Python 只传 X-Conduit-Token: no-token
// 占位），与图片链路 prepare 后再补 sentinel 头的做法不同。
func (c *Client) prepareSearchConversation(ctx context.Context, prompt, model string) (string, error) {
	payload := map[string]any{
		"action":                "next",
		"fork_from_shared_post": false,
		"parent_message_id":     "client-created-root",
		"model":                 model,
		"client_prepare_state":  "success",
		"timezone_offset_min":   -480,
		"timezone":              "Asia/Shanghai",
		"conversation_mode":     map[string]any{"kind": "primary_assistant"},
		"system_hints":          []any{"search"},
		"partial_query": map[string]any{
			"id":      newUUID(),
			"author":  map[string]any{"role": "user"},
			"content": map[string]any{"content_type": "text", "parts": []any{prompt}},
		},
		"supports_buffering":     true,
		"supported_encodings":    []any{"v1"},
		"client_contextual_info": map[string]any{"app_name": "chatgpt.com"},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	path := "/backend-api/f/conversation/prepare"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+path, newBytesReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header = c.headers(path, map[string]string{
		"Accept":          "*/*",
		"Content-Type":    "application/json",
		"X-Conduit-Token": "no-token",
	})

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if resp.StatusCode >= 400 {
		return "", NewStatusError(resp.StatusCode, retryAfterFromBody(responseBody),
			fmt.Sprintf("%s failed: status=%d body=%s", path, resp.StatusCode, truncate(string(responseBody), 300)))
	}

	var decoded map[string]any
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return "", fmt.Errorf("%s: invalid json: %w", path, err)
	}
	conduitToken := stringOfValue(decoded["conduit_token"])
	if conduitToken == "" {
		return "", fmt.Errorf("missing conduit_token")
	}
	return conduitToken, nil
}

// runSearchConversation 复刻 _run_search_conversation（:1861-1912）：
// 带 sentinel token 与 conduit_token 发起 SSE 请求，从事件流里找 conversation_id。
func (c *Client) runSearchConversation(ctx context.Context, prompt, conduitToken, model string) (string, error) {
	rawReq, err := c.chatRequirements(ctx)
	if err != nil {
		return "", err
	}
	requirements := protocol.Requirements{
		Token:          rawReq.Token,
		ProofToken:     rawReq.ProofToken,
		TurnstileToken: rawReq.TurnstileToken,
		SOToken:        rawReq.SOToken,
		Raw:            rawReq.Raw,
	}

	payload := map[string]any{
		"action": "next",
		"messages": []any{map[string]any{
			"id":          newUUID(),
			"author":      map[string]any{"role": "user"},
			"create_time": float64(time.Now().UnixNano()) / 1e9,
			"content":     map[string]any{"content_type": "text", "parts": []any{prompt}},
			"metadata": map[string]any{
				"developer_mode_connector_ids": []any{},
				"selected_github_repos":        []any{},
				"selected_all_github_repos":    false,
				"system_hints":                 []any{"search"},
				"serialization_metadata":       map[string]any{"custom_symbol_offsets": []any{}},
			},
		}},
		"parent_message_id":        "client-created-root",
		"model":                    model,
		"client_prepare_state":     "success",
		"timezone_offset_min":      -480,
		"timezone":                 "Asia/Shanghai",
		"conversation_mode":        map[string]any{"kind": "primary_assistant"},
		"enable_message_followups": true,
		// 顶层的 system_hints 是空数组（只有 message.metadata 里带 "search"）——
		// 别"顺手"改成 ["search"]，上游据此判断是否是搜索意图，改了就变普通对话。
		"system_hints":                         []any{},
		"supports_buffering":                   true,
		"supported_encodings":                  []any{"v1"},
		"force_use_search":                     true,
		"client_reported_search_source":        "conversation_composer_web_icon",
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": 36,
			"page_height":       925,
			"page_width":        886,
			"pixel_ratio":       2,
			"screen_height":     1440,
			"screen_width":      2560,
			"app_name":          "chatgpt.com",
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	path := "/backend-api/f/conversation"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+path, newBytesReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header = c.imageHeaders(path, requirements, conduitToken, "text/event-stream")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return "", NewStatusError(resp.StatusCode, retryAfterFromBody(preview),
			fmt.Sprintf("%s failed: status=%d body=%s", path, resp.StatusCode, truncate(string(preview), 300)))
	}

	// 复用图片链路的 SSE 解析：payload 逐条送出，遇到 [DONE] 自动收尾。
	// 一定要把 channel 读干净（直到关闭），否则 goroutine 会卡在发送上。
	payloads := make(chan string, 64)
	errs := make(chan error, 1)
	go func() {
		defer close(payloads)
		defer close(errs)
		defer resp.Body.Close()
		if readErr := readSSEPayloads(resp.Body, payloads); readErr != nil {
			errs <- readErr
		}
	}()

	conversationID := ""
	for payload := range payloads {
		if conversationID == "" {
			conversationID = protocol.FindSearchValue(payload, "conversation_id")
		}
	}
	var readErr error
	for err := range errs {
		readErr = err
	}
	if readErr != nil {
		return "", readErr
	}
	if conversationID == "" {
		return "", fmt.Errorf("conversation_id not found in stream")
	}
	return conversationID, nil
}

// waitSearchResult 复刻 _wait_search_result（:1914-1936）。
//
// 结束条件（任一）：
//   - answer 非空且 status ∈ SearchDoneStatus
//   - answer 连续两次与上次相同（stable_hits >= 2，即连续 3 次一致）
//   - 超时后仍有上一次可用结果 → 返回它；没有任何结果才报超时
func (c *Client) waitSearchResult(
	ctx context.Context,
	conversationID string,
	options searchOptions,
) (*protocol.SearchResult, error) {
	deadline := time.Now().Add(options.timeout)
	var last *protocol.SearchResult
	lastAnswer := ""
	stableHits := 0

	for time.Now().Before(deadline) {
		conversation, err := c.getSearchConversation(ctx, conversationID)
		if err != nil {
			// 容忍的状态码：保留上一次结果继续下一轮；其余立刻冒泡
			// （401/403 是账号失效，重试没有意义）。
			if !searchToleratedStatuses[protocol.StatusCodeOf(err)] {
				return nil, err
			}
		} else {
			result := protocol.ExtractSearchResult(conversationID, conversation)
			last = &result
		}

		if last != nil && last.Answer != "" {
			if protocol.SearchDoneStatus[last.Status] {
				return last, nil
			}
			if last.Answer == lastAnswer {
				stableHits++
			} else {
				stableHits = 0
			}
			lastAnswer = last.Answer
			if stableHits >= 2 {
				return last, nil
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(options.pollInterval):
		}
	}

	if last != nil {
		return last, nil
	}
	return nil, fmt.Errorf("timed out waiting for search result: %s", conversationID)
}

// getSearchConversation 复刻 _get_search_conversation（:1938-1945）。
//
// 与普通 GET 的差别：Referer 指向 /c/{id}，且 X-OpenAI-Target-Route 必须是
// 字面量 {conversation_id}（不是真实 id）—— 与 DeleteConversation 同一约定。
func (c *Client) getSearchConversation(ctx context.Context, conversationID string) (map[string]any, error) {
	path := "/backend-api/conversation/" + conversationID
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header = c.headers(path, map[string]string{
		"Accept":                "*/*",
		"Referer":               BaseURL + "/c/" + conversationID,
		"X-OpenAI-Target-Route": "/backend-api/conversation/{conversation_id}",
	})

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, NewStatusError(resp.StatusCode, retryAfterFromBody(body),
			fmt.Sprintf("%s failed: status=%d body=%s", path, resp.StatusCode, truncate(string(body), 300)))
	}

	var conversation map[string]any
	if err := json.Unmarshal(body, &conversation); err != nil {
		return nil, fmt.Errorf("%s: invalid json: %w", path, err)
	}
	return conversation, nil
}
