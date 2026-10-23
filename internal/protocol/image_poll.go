package protocol

import (
	"context"
	"math"
	"math/rand"
	"time"
)

// ImageUpstream 是图片链路需要的上游能力（比 Upstream 多出轮询与下载）。
type ImageUpstream interface {
	Upstream
	// GetConversation 拉一次对话文档（轮询用）。
	GetConversation(ctx context.Context, conversationID string) (map[string]any, error)
	// QueryTasks 查 /backend-api/tasks，用于拿结构化错误。
	QueryTasks(ctx context.Context, conversationID string) ([]map[string]any, error)
	// ResolveImageURLs 把 file_id / sediment_id 解析成可下载 URL。
	ResolveImageURLs(ctx context.Context, conversationID string, fileIDs, sedimentIDs []string) ([]string, error)
	// DownloadImage 下载图片字节。
	DownloadImage(ctx context.Context, url string) ([]byte, error)
	// FindConversationByPrompt 在最近对话里按 prompt 找回 conversation_id。
	FindConversationByPrompt(ctx context.Context, prompt string, since time.Time) (string, error)
	// DeleteConversation 删除对话（成功后清理用）。
	DeleteConversation(ctx context.Context, conversationID string) error
	// StreamImage 走图片专用链路（/backend-api/f/conversation 系列）。
	//
	// 为什么不复用 Upstream.DoStream：图片链路要先用 /f/conversation/prepare
	// 换 conduit_token，再带 system_hints=["picture_v2"] 打 /f/conversation。
	// 走文本端点 /backend-api/conversation 会稳定被上游拒绝。
	StreamImage(ctx context.Context, request ImageStreamRequest) (<-chan string, <-chan error, error)
}

// ImageStreamRequest 是图片链路需要的输入。
type ImageStreamRequest struct {
	Prompt string
	Model  string
	Images [][3]string
	// Size / Quality 只被 codex 链路使用（tools[0].size / tools[0].quality）。
	//
	// web 链路把它们拼进提示词（promptWithImageHints），所以这里不需要再传；
	// codex 的 Responses 协议把它们作为工具参数发送，拼进提示词反而会干扰模型。
	Size     string
	Quality  string
	Progress func(step string)
	// DefaultUpstreamModel / DefaultThinkingEffort 由 api 层从配置注入。
	//
	// gpt-image-2 的底层上游模型由 default_upstream_model_name 决定
	// （默认 gpt-5-5），思考强度由 default_thinking_effort 决定。
	// 不注入的话这两个配置项保存成功却永不生效（实测）。
	DefaultUpstreamModel  string
	DefaultThinkingEffort string
}

// PollOptions 是轮询行为配置（对应 config 里的几个 image_* 字段）。
type PollOptions struct {
	TimeoutSecs        float64
	IntervalSecs       float64
	InitialWaitSecs    float64
	SettleEnabled      bool
	CheckBeforeHit     bool
	SettleSecs         float64
	RemoveConversation func(conversationID string, success bool)
}

// PollResult 是轮询产出。
type PollResult struct {
	FileIDs     []string
	SedimentIDs []string
	TaskError   string
}

// PollImageResults 复刻 _poll_image_results。
//
// 行为要点（每一条都对应一段上游的真实脾气）：
//  1. 首次轮询前先等 initial_wait + jitter（默认 10s）。上游出图约 30s，
//     立刻轮询会撞 429 —— 对话文档还没提交。
//  2. 已有初始 id 时改用 settle 等待（默认 2s），因为此时只差「确认落定」。
//  3. 上游 429/5xx 指数退避（上限 16s），尊重 Retry-After。
//  4. 找到 id 后若 check_before_hit 开启，要等「连续两次看到同一组 id」才算落定。
//  5. 对话文本里出现内容政策违规时立即失败，不等到超时。
//
// 与 Python 的差异：这里用 ctx 取消替代「检查剩余时间」，语义等价但更可组合。
func PollImageResults(
	ctx context.Context,
	upstream ImageUpstream,
	conversationID string,
	opts PollOptions,
	initialFileIDs, initialSedimentIDs []string,
) (PollResult, error) {
	result := PollResult{
		FileIDs:     append([]string{}, initialFileIDs...),
		SedimentIDs: append([]string{}, initialSedimentIDs...),
	}
	hasInitial := len(result.FileIDs) > 0 || len(result.SedimentIDs) > 0

	timeout := opts.TimeoutSecs
	if timeout <= 0 {
		timeout = 120
	}
	interval := opts.IntervalSecs
	if interval <= 0 {
		interval = 10
	}

	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	// 首次等待
	if hasInitial && opts.SettleEnabled {
		wait := math.Min(opts.SettleSecs, timeout)
		if wait > 0 {
			if !sleepCtx(pollCtx, time.Duration(wait*float64(time.Second))) {
				return result, pollTimeoutError(conversationID, timeout, result.TaskError)
			}
		}
	} else if opts.InitialWaitSecs > 0 {
		jitter := rand.Float64() * math.Min(2.0, opts.InitialWaitSecs*0.2)
		wait := math.Min(opts.InitialWaitSecs+jitter, timeout)
		if wait > 0 {
			if !sleepCtx(pollCtx, time.Duration(wait*float64(time.Second))) {
				return result, pollTimeoutError(conversationID, timeout, result.TaskError)
			}
		}
	}

	var lastHitKey string
	attempt := 0
	for {
		if pollCtx.Err() != nil {
			return result, pollTimeoutError(conversationID, timeout, result.TaskError)
		}
		attempt++

		// 每次都查 tasks，但只记录不中断：上游会把工具调用的 JSON 参数
		// 标记成 is_error，而图片其实还在异步生成。
		if tasks, err := upstream.QueryTasks(pollCtx, conversationID); err == nil {
			for _, task := range tasks {
				if message := TaskErrorMessage(task); message != "" {
					result.TaskError = message
				}
			}
		}

		conversation, err := upstream.GetConversation(pollCtx, conversationID)
		if err != nil {
			if status := httpStatusOf(err); status == 429 || status >= 500 {
				if !retrySleep(pollCtx, &attempt, retryAfterOf(err)) {
					return result, pollTimeoutError(conversationID, timeout, result.TaskError)
				}
				continue
			}
			if isNetworkError(err) {
				if !retrySleep(pollCtx, &attempt, nil) {
					return result, pollTimeoutError(conversationID, timeout, result.TaskError)
				}
				continue
			}
			return result, err
		}

		for _, record := range ExtractImageToolRecords(conversation) {
			for _, id := range record.FileIDs {
				appendUnique(&result.FileIDs, id)
			}
			for _, id := range record.SedimentIDs {
				appendUnique(&result.SedimentIDs, id)
			}
		}

		// 没有 id 但对话里出现内容政策违规 → 立即失败，不等超时。
		if len(result.FileIDs) == 0 && len(result.SedimentIDs) == 0 {
			if policyMessage := FindContentPolicyError(conversation); policyMessage != "" {
				return result, &ImageContentPolicyError{
					Message:        policyMessage,
					ConversationID: conversationID,
				}
			}
		}

		if len(result.FileIDs) > 0 || len(result.SedimentIDs) > 0 {
			if !opts.CheckBeforeHit {
				return result, nil
			}
			hitKey := joinKey(result.FileIDs) + "|" + joinKey(result.SedimentIDs)
			if lastHitKey == hitKey {
				return result, nil
			}
			lastHitKey = hitKey
			if !opts.SettleEnabled {
				return result, nil
			}
			// 二次确认：等一小会儿再看一次，确保 id 不会继续增长。
			if !sleepCtx(pollCtx, time.Duration(opts.SettleSecs*float64(time.Second))) {
				return result, nil
			}
			continue
		}

		if !sleepCtx(pollCtx, time.Duration(interval*float64(time.Second))) {
			return result, pollTimeoutError(conversationID, timeout, result.TaskError)
		}
	}
}

// pollTimeoutError 复刻超时文案（含调参提示，运维据此定位）。
func pollTimeoutError(conversationID string, timeout float64, taskError string) error {
	message := "ChatGPT 生图超时（已等待 " + itoaFloat(timeout) + " 秒）。" +
		"当前超时阈值可在 config.json 中调大 image_poll_timeout_secs，" +
		"也可能是账号被限流或生图队列拥堵导致。"
	return &ImagePollTimeoutError{
		Message:        message,
		ConversationID: conversationID,
		TaskError:      taskError,
	}
}

// retrySleep 指数退避（上限 16s）+ 抖动，尊重 Retry-After。
func retrySleep(ctx context.Context, attempt *int, retryAfter *int) bool {
	base := 0.0
	if retryAfter != nil {
		base = float64(*retryAfter)
	} else {
		exponent := *attempt
		if exponent > 4 {
			exponent = 4
		}
		base = math.Min(math.Pow(2, float64(exponent)), 16)
	}
	backoff := base + rand.Float64()*0.5
	return sleepCtx(ctx, time.Duration(backoff*float64(time.Second)))
}

// sleepCtx 睡指定时长；ctx 提前结束则返回 false。
func sleepCtx(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func joinKey(values []string) string {
	out := ""
	for index, value := range values {
		if index > 0 {
			out += ","
		}
		out += value
	}
	return out
}

func itoaFloat(value float64) string {
	if value == math.Trunc(value) {
		return itoa(int(value))
	}
	return formatFloatValue(value)
}
