package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

// TextUpstreamFactory 为一次文本请求构造上游客户端。
// 由 main 注入（避免 api 直接依赖 upstream 的构造细节）。
type TextUpstreamFactory func(ctx context.Context, accessToken string) (protocol.Upstream, func(), error)

// chatCacheSettings 把配置转成 protocol 需要的缓存设置。
func (d *Deps) chatCacheSettings() protocol.CacheSettings {
	settings := d.Config.ChatCompletionCacheSettings()
	return protocol.CacheSettings{
		Enabled:                truthyValue(settings["enabled"]),
		TTLSeconds:             intOf(settings["ttl_seconds"]),
		MaxEntries:             intOf(settings["max_entries"]),
		DedupeInflight:         truthyValue(settings["dedupe_inflight"]),
		StreamCache:            truthyValue(settings["stream_cache"]),
		NormalizeMessages:      truthyValue(settings["normalize_messages"]),
		DropAdjacentDuplicates: truthyValue(settings["drop_adjacent_duplicates"]),
		DropAssistantHistory:   truthyValue(settings["drop_assistant_history"]),
	}
}

func intOf(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	default:
		return 0
	}
}

// registerChatCompletions 挂载 /v1/chat/completions。
//
// 支持三种走向（与 Python 一致）：
//   - 图片请求 → 走生图链路并把结果转成 chat completion
//   - 网页搜索请求 → 走搜索链路
//   - 其余 → 文本对话
func (d *Deps) registerChatCompletions(r *gin.Engine) {
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		identity, authErr := d.RequireIdentityCtx(c)
		if authErr != nil {
			writeErr(c, authErr)
			return
		}
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}

		// 每次 /v1 调用都记一条 call 日志：日志页默认按 type=call 过滤，
		// 不写的话首屏恒为空。
		//
		// 图片形态与非图片形态共用同一个摘要：Python 的 LoggedCall 对
		// /v1/chat/completions 也只有一个 summary（api/ai.py:127-134），
		// 具体走向靠 detail.endpoint + urls 区分。
		call := d.beginCall("文本对话", c.FullPath(),
			strings.TrimSpace(stringValueOf(body["model"])), identity.Name)
		outcome := &chatCallOutcome{}
		defer func() { call.finish(outcome.err, outcome.accountEmail, outcome.urls) }()

		// 图片请求走生图链路并把结果转成 chat completion（Python
		// openai_v1_chat_complete.handle 的图片分支）。
		if isImageChatRequest(body) {
			d.handleImageChat(c, body, outcome)
			return
		}

		// 网页搜索：只有「支持的工具类型」才走搜索链路；
		// 含不支持的工具时落到文本链路（那里会注入 TOOL_UNAVAILABLE 提示），
		// 与 Python 的 `and not has_unsupported_tools(...)` 一致。
		if protocol.IsWebSearchChatRequest(body) && !protocol.HasUnsupportedTools(body, protocol.WebSearchToolTypes) {
			d.handleWebSearchChat(c, body, outcome)
			return
		}

		d.handleTextChat(c, body)
	})

	// Anthropic Messages 兼容：鉴权同时接受 Authorization 与 x-api-key。
	r.POST("/v1/messages", func(c *gin.Context) {
		authorization := c.GetHeader("Authorization")
		if authorization == "" {
			if apiKey := c.GetHeader("x-api-key"); apiKey != "" {
				authorization = "Bearer " + apiKey
			}
		}
		identity, authErr := d.RequireIdentity(authorization)
		if authErr != nil {
			writeErr(c, authErr)
			return
		}
		c.Set("key_name", identity.Name)
		body, bodyErr := decodeJSONBody(c)
		if bodyErr != nil {
			writeErr(c, bodyErr)
			return
		}
		call := d.beginCall("Anthropic 对话", c.FullPath(),
			strings.TrimSpace(stringValueOf(body["model"])), identity.Name)
		defer func() { call.finish(nil, "", nil) }()
		d.handleAnthropicMessages(c, body)
	})
}

// handleAnthropicMessages 走 Anthropic 协议链路。
func (d *Deps) handleAnthropicMessages(c *gin.Context, body map[string]any) {
	payload := protocol.PreprocessPayload(body)
	model := strings.TrimSpace(stringValueOf(payload["model"]))
	if model == "" {
		model = "auto"
	}
	tools := payload["tools"]

	messages := anthropicMessages(payload["messages"], payload["system"])
	stream := truthyValue(body["stream"])

	var text strings.Builder
	produce := func(ctx context.Context, onDelta func(string)) error {
		return d.runTextConversation(ctx, model, messages, "", onDelta)
	}

	if stream {
		chunks, err := protocol.StreamTextChatCompletion(
			model, protocol.CompletionID(), protocol.NowUnix(),
			func(ctx context.Context, onDelta func(string)) error {
				return produce(ctx, func(delta string) {
					text.WriteString(delta)
					onDelta(delta)
				})
			},
		)
		if err != nil {
			writeErr(c, mapProtocolError(err))
			return
		}
		events := protocol.StreamAnthropicEvents(
			chunks, model,
			protocol.CountMessageTextTokens(messages, model),
			func(value string) int { return protocol.CountTextTokens(value, model) },
			tools,
		)
		writeAnthropicSSE(c, events)
		return
	}

	if err := produce(context.Background(), func(delta string) { text.WriteString(delta) }); err != nil {
		writeErr(c, mapProtocolError(err))
		return
	}
	content := text.String()
	c.JSON(http.StatusOK, protocol.MessageResponse(
		model, content,
		protocol.CountMessageTextTokens(messages, model),
		protocol.CountTextTokens(content, model),
		tools,
	))
}

// anthropicMessages 把 Anthropic 的 messages + system 转成内部消息序列。
//
// system 支持字符串与文本块数组两种形态，都要转成 role=system 的消息。
func anthropicMessages(rawMessages any, system any) []map[string]any {
	out := make([]map[string]any, 0, 4)

	if text := protocol.MessageText(system); strings.TrimSpace(text) != "" {
		out = append(out, map[string]any{"role": "system", "content": text})
	}

	list, ok := rawMessages.([]any)
	if !ok {
		return out
	}
	for _, item := range list {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.TrimSpace(stringValueOf(message["role"]))
		if role == "" {
			role = "user"
		}
		out = append(out, map[string]any{
			"role":    role,
			"content": protocol.MessageText(message["content"]),
		})
	}
	return out
}

// writeAnthropicSSE 按 Anthropic 事件流格式写出。
//
// 格式与 Python 的 anthropic_sse_stream 一致：每个事件带 event: 行 + data: 行。
func writeAnthropicSSE(c *gin.Context, events []map[string]any) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	writer := c.Writer
	for _, event := range events {
		eventType := stringValueOf(event["type"])
		if eventType == "" {
			eventType = "message_delta"
		}
		payload, err := marshalJSON(event)
		if err != nil {
			continue
		}
		_, _ = writer.WriteString("event: " + eventType + "\n")
		_, _ = writer.WriteString("data: " + payload + "\n\n")
		writer.Flush()
	}
}

// handleTextChat 走文本对话链路（含缓存与流式）。
func (d *Deps) handleTextChat(c *gin.Context, body map[string]any) {
	settings := d.chatCacheSettings()
	model, messages, err := protocol.TextChatParts(body, settings)
	if err != nil {
		writeErr(c, Errorf(http.StatusBadRequest, err.Error()))
		return
	}

	thinkingEffort := protocol.ThinkingEffortFromBody(body)
	stream := truthyValue(body["stream"])
	key := protocol.CacheKey(body, messages, stream)

	produce := func(ctx context.Context, onDelta func(string)) error {
		return d.runTextConversation(ctx, model, messages, thinkingEffort, onDelta)
	}

	if stream {
		chunks, computeErr := d.ChatCache.GetOrComputeStream(key, func() ([]map[string]any, error) {
			completionID := protocol.CompletionID()
			created := protocol.NowUnix()
			return protocol.StreamTextChatCompletion(model, completionID, created, produce)
		})
		if computeErr != nil {
			writeErr(c, mapProtocolError(computeErr))
			return
		}
		writeSSEChunks(c, chunks)
		return
	}

	response, computeErr := d.ChatCache.GetOrComputeResponse(key, func() (map[string]any, error) {
		var text strings.Builder
		if runErr := produce(context.Background(), func(delta string) {
			text.WriteString(delta)
		}); runErr != nil {
			return nil, runErr
		}
		content := text.String()
		return protocol.ChatCompletionResponse(
			model, content, protocol.NowUnix(),
			protocol.CountMessageTextTokens(messages, model),
			protocol.CountMessageImageTokens(messages, model),
			protocol.CountTextTokens(content, model),
			nil,
		), nil
	})
	if computeErr != nil {
		writeErr(c, mapProtocolError(computeErr))
		return
	}
	c.JSON(http.StatusOK, response)
}

// runTextConversation 取一个文本账号并跑完一次对话。
func (d *Deps) runTextConversation(
	ctx context.Context,
	model string,
	messages []map[string]any,
	thinkingEffort string,
	onDelta func(string),
) error {
	if d.TextUpstream == nil {
		return fmt.Errorf("text upstream factory is not configured")
	}

	// 内容检查：敏感词 + AI 审核。不检查等于设置页里的配置形同虚设。
	if filterErr := d.checkContentMessages(ctx, messages); filterErr != nil {
		return filterErr
	}

	accountTypes, allowAnonymous := d.Models.RouteForModel(ctx, model)
	var route map[string]bool
	if len(accountTypes) > 0 {
		route = accountTypes
	}

	token, err := d.Accts.GetTextAccessToken(nil, route, allowAnonymous)
	if err != nil {
		return err
	}

	upstream, closeFn, err := d.TextUpstream(ctx, token)
	if err != nil {
		return err
	}
	defer closeFn()

	if token != "" {
		defer d.Accts.MarkTextUsed(token)
	}

	return protocol.StreamTextDeltas(ctx, upstream, protocol.ConversationTextRequest{
		Model:          model,
		Messages:       messages,
		ThinkingEffort: thinkingEffort,
		History:        protocol.HistoryTexts(messages),
	}, onDelta)
}

// mapProtocolError 把协议层错误映射成 HTTPError。
func mapProtocolError(err error) *HTTPError {
	message := err.Error()
	// 号池没号是可预期的运营状态，映射成 429 而非 500。
	if strings.Contains(message, "no available image quota") {
		return Errorf(http.StatusTooManyRequests, "no available image quota")
	}
	if strings.Contains(message, "not available to any active account") {
		return Errorf(http.StatusBadRequest, message)
	}
	return Errorf(http.StatusBadGateway, message)
}

// isImageChatRequest 复刻 utils/helper.py 的 is_image_chat_request：
// isImageChatRequest 复刻 Python 的 is_image_chat_request（utils/helper.py:134-139）。
//
// 判定条件只有两个：model 是图片模型，或 modalities 里含 "image"。
//
// **不能扫描 messages**：带 image_url 的 chat 请求是「视觉对话」（让模型看图后
// 用文本回答），不是「生成图片」。多扫 messages 会把所有视觉请求误判成图片
// 生成并返回 501，视觉对话 100% 不可用（已实测）。Python 不扫，这里也不扫。
func isImageChatRequest(body map[string]any) bool {
	model := strings.TrimSpace(stringValueOf(body["model"]))
	if protocol.IsSupportedImageModel(model) {
		return true
	}
	modalities, ok := body["modalities"].([]any)
	if !ok {
		return false
	}
	for _, item := range modalities {
		if strings.ToLower(strings.TrimSpace(stringValueOf(item))) == "image" {
			return true
		}
	}
	return false
}

// writeSSEChunks 把 chunk 列表按 OpenAI SSE 格式写出。
//
// 格式与 Python 的 sse_json_stream 一致：先发 ": stream-open"，再逐条 data:，
// 最后 data: [DONE]。少了 stream-open 某些客户端会认为连接异常。
func writeSSEChunks(c *gin.Context, chunks []map[string]any) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	writer := c.Writer
	_, _ = writer.WriteString(": stream-open\n\n")
	for _, chunk := range chunks {
		payload, err := marshalJSON(chunk)
		if err != nil {
			continue
		}
		_, _ = writer.WriteString("data: " + payload + "\n\n")
		writer.Flush()
	}
	_, _ = writer.WriteString("data: [DONE]\n\n")
	writer.Flush()
}

// service 包的引用保持（GetTextAccessToken 的参数类型来自 service）。
var _ = service.RoleUser

// writeSSEFromChannel 边从 channel 读 chunk 边写 SSE，读到关闭为止。
//
// 与 writeSSEChunks 的区别：后者接收切片，意味着所有 chunk 必须先全部
// 生成完才能开始写。长请求（生图几十秒、文本长回答）期间客户端收不到
// 任何字节，nginx / Cloudflare 的 60s 空闲超时会直接切断连接 ——
// 用户看到的是「卡住然后失败」。
//
// channel 版在上游产生第一个 chunk 时就 flush 出去，连接全程有数据流动。
// 每写一条都 Flush：gin 默认缓冲，不 flush 等于还是批量写。
func writeSSEFromChannel(c *gin.Context, chunks <-chan map[string]any) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	writer := c.Writer
	_, _ = writer.WriteString(": stream-open\n\n")
	writer.Flush()

	for chunk := range chunks {
		payload, err := marshalJSON(chunk)
		if err != nil {
			continue
		}
		_, _ = writer.WriteString("data: " + payload + "\n\n")
		writer.Flush()
	}

	_, _ = writer.WriteString("data: [DONE]\n\n")
	writer.Flush()
}
