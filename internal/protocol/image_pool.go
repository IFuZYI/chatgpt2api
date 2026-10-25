package protocol

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ImagePool 负责把 n 张图分发到不同账号并行生成。
//
// 复刻 Python 的 stream_image_outputs_with_pool，但用 goroutine + channel
// 而不是线程池（ADR-0007）。关键行为对齐：
//   - n <= 1 时不并发（避免无谓开销）
//   - image_parallel_generation=false 时串行
//   - 并行时按**完成顺序**收集，且「低索引失败不阻塞高索引成功」
//   - 全部失败才返回错误；部分成功只记警告
type ImagePool struct {
	Parallel bool
	// Acquire 取一个账号槽位并返回一个 generator。
	// 返回的 release 必须调用（内部用 defer）。
	Acquire func(ctx context.Context, index int) (*ImageGenerator, func(), error)
	// Generate 执行一次生成。nil 时用 generator.Generate。
	// 抽成字段只为让测试能注入失败序列，验证换号重试。
	Generate func(ctx context.Context, generator *ImageGenerator, request ImageRequest) ([]ImageOutput, error)
}

// 四类重试预算，逐字对齐 Python conversation.py:1301-1307。
const (
	// maxTextReplyRetries 模型返回文本而非图片。
	maxTextReplyRetries = 3
	// maxTLSRetries TLS 连接错误。
	maxTLSRetries = 3
	// maxConnTimeoutRetries 连接超时（同账号短退避）。
	maxConnTimeoutRetries = 3
	// maxPollTimeoutRetries 轮询超时（换账号）。
	maxPollTimeoutRetries = 4
)

// retryKind 是一次失败属于哪一类预算。
type retryKind int

const (
	retryNone retryKind = iota
	retryTextReply
	retryTLS
	retryConnTimeout
	retryPollTimeout
)

// classifyRetry 把错误归类到预算桶。未识别的错误不重试（快速失败）。
//
// Python 用异常类型判断（ImagePollTimeoutError 等）；Go 只有错误码/文案，
// 所以按 Code 优先、文案兜底。
//
// 文案兜底必须同时看「改写后的对外文案」和「Cause 里的原文」：
// wrapImageError 会把 curl: (35)/(28) 这类标记替换成固定句子，
// 只看 Message 会让 TLS/连接超时两个桶永远空转。
func classifyRetry(err error) retryKind {
	if err == nil {
		return retryNone
	}
	var genErr *ImageGenerationError
	if errors.As(err, &genErr) {
		switch genErr.Code {
		case "upstream_text_reply", "text_reply":
			return retryTextReply
		case "image_poll_timeout":
			return retryPollTimeout
		}
		if genErr.StatusCode == 504 {
			return retryPollTimeout
		}
		// Cause 优先：原文才带 curl/SSL 标记。
		if genErr.Cause != nil {
			if kind := classifyRetryText(genErr.Cause.Error()); kind != retryNone {
				return kind
			}
		}
	}
	return classifyRetryText(err.Error())
}

// classifyRetryText 按文案判定重试类别。
func classifyRetryText(text string) retryKind {
	if IsModelTextReplyInsteadOfImage(text) {
		return retryTextReply
	}
	if IsTLSConnectionError(text) {
		return retryTLS
	}
	if IsConnectionTimeoutError(text) {
		return retryConnTimeout
	}
	return retryNone
}

// generateOnce 走可注入的 Generate 或默认方法。
func (p *ImagePool) generateOnce(ctx context.Context, generator *ImageGenerator, request ImageRequest) ([]ImageOutput, error) {
	if p.Generate != nil {
		return p.Generate(ctx, generator, request)
	}
	return generator.Generate(ctx, request)
}

// attemptOnce 执行一次「取号 → 生成 → 释放」。
//
// release 必须用 defer：generateOnce 里任何 panic 都不能把账号槽位永久留在
// inflight 状态（旧 runSingle 就是这么做的，改成重试循环后一度丢了这个保证）。
func (p *ImagePool) attemptOnce(ctx context.Context, request ImageRequest, index int) (outputs []ImageOutput, err error) {
	generator, release, acquireErr := p.Acquire(ctx, index)
	if acquireErr != nil {
		return nil, acquireErr
	}
	defer release()
	return p.generateOnce(ctx, generator, request)
}

// messageOnlyOutputs 找出这次「没有错误」的返回是否其实是文本回复。
//
// 真实 Generate 对「模型返回文本而非图片」不抛错，而是产出 message output
// 并返回 nil error（image_generate.go 的多个分支）。只靠错误分类永远进不了
// text-reply 预算，所以这里补一层：有 message、没有任何 result、且文案带
// referenced_image_ids 标记（Python is_model_text_reply_instead_of_image 的
// 同一判据，conversation.py:126）才算文本回复。
//
// 不能放宽成「全部是 message」：内容政策拒绝也是 message，Python 对它不重试，
// 放宽会让拒图请求白烧 4 次额度并反复触犯上游。
//
// ⚠️ 与 Python 的有意发散：异步任务路径（message_as_error=false）在 Python 里
// 拿到 message 就 return（conversation.py:1367），不再重试；同步路径
// （message_as_error=true）才会因 referenced_image_ids 进入重试
// （conversation.py:1440）。Go 两条路径都重试，因为异步任务同样会因一个
// 抽风的账号白丢一张图，而 referenced_image_ids 已是 Python 自己的「可重试」
// 判据。待上游行为稳定后可回退成只在同步路径启用。
func messageOnlyOutputs(outputs []ImageOutput) (ImageOutput, bool) {
	var message ImageOutput
	found := false
	for _, output := range outputs {
		if output.Kind == ImageKindResult {
			return ImageOutput{}, false
		}
		if output.Kind == ImageKindMessage && !found {
			message = output
			found = true
		}
	}
	if !found || !IsModelTextReplyInsteadOfImage(message.Text) {
		return ImageOutput{}, false
	}
	return message, true
}

// generateWithRetry 按四类预算换号重试，返回最终输出与错误。
//
// 与 Python 一致：每次重试都重新 Acquire（换号），且 release 必须先调，
// 否则槽位泄漏、账号永远处于 inflight。
func (p *ImagePool) generateWithRetry(ctx context.Context, request ImageRequest, index int) ([]ImageOutput, error) {
	var (
		outputs   []ImageOutput
		lastErr   error
		textReply int
		tlsRetry  int
		connRetry int
		pollRetry int
	)

	for {
		generated, genErr := p.attemptOnce(ctx, request, index)

		if genErr == nil {
			// 无错误但产出的是「文本回复」：按 text-reply 预算换号重试。
			if message, isTextReply := messageOnlyOutputs(generated); isTextReply {
				outputs = generated
				textReply++
				if textReply > maxTextReplyRetries {
					// 把最后一次 message 的现场带进错误，保住 account_email 与
					// conversation_id —— 任务详情和「继续等待」都依赖后者。
					return outputs, textReplyExhaustedError(&ImageGenerationError{
						Message:        message.Text,
						AccountEmail:   message.AccountEmail,
						ConversationID: message.ConversationID,
					})
				}
				continue
			}
			return generated, nil
		}

		outputs = generated
		lastErr = genErr

		switch classifyRetry(genErr) {
		case retryTextReply:
			textReply++
			if textReply > maxTextReplyRetries {
				return outputs, textReplyExhaustedError(genErr)
			}
			continue
		case retryTLS:
			tlsRetry++
			if tlsRetry > maxTLSRetries {
				return outputs, genErr
			}
			continue
		case retryConnTimeout:
			connRetry++
			if connRetry > maxConnTimeoutRetries {
				return outputs, genErr
			}
			continue
		case retryPollTimeout:
			pollRetry++
			if pollRetry > maxPollTimeoutRetries {
				return outputs, genErr
			}
			continue
		default:
			// 未识别错误：快速失败（Python 同样只对四类重试）。
			return outputs, lastErr
		}
	}
}

// textReplyExhaustedError 构造预算耗尽后的错误（文案逐字对齐 Python）。
func textReplyExhaustedError(cause error) error {
	genErr := &ImageGenerationError{
		Message: "Image generation failed: the upstream model returned a text description " +
			"instead of generating an image. Please try again later.",
		StatusCode: 502,
		ErrorType:  "server_error",
		Code:       "upstream_text_reply",
	}
	var previous *ImageGenerationError
	if errors.As(cause, &previous) {
		genErr.AccountEmail = previous.AccountEmail
		genErr.ConversationID = previous.ConversationID
	}
	return genErr
}

// PoolResult 是一次并行生成的结果。
type PoolResult struct {
	Outputs []ImageOutput
	// Failures 记录失败的索引与原因（部分失败时用于告警）。
	Failures map[int]string
}

// Run 执行 n 张图的生成。
//
// 返回值语义与 Python 一致：
//   - 至少一张成功 → 返回 (result, nil)，失败信息放在 result.Failures
//   - 全部失败 → 返回 (result, error)，error 是最后一条失败原因
func (p *ImagePool) Run(ctx context.Context, request ImageRequest) (PoolResult, error) {
	total := request.N
	if total <= 1 {
		return p.runSingle(ctx, request, 1, 1)
	}

	if !p.Parallel {
		return p.runSerial(ctx, request)
	}
	return p.runParallel(ctx, request)
}

func (p *ImagePool) runSingle(ctx context.Context, request ImageRequest, index, total int) (PoolResult, error) {
	outputs, err := p.generateWithRetry(ctx, request, index)
	if err != nil {
		return PoolResult{Outputs: outputs}, err
	}
	return PoolResult{Outputs: outputs}, nil
}

func (p *ImagePool) runSerial(ctx context.Context, request ImageRequest) (PoolResult, error) {
	result := PoolResult{Failures: map[int]string{}}
	for index := 1; index <= request.N; index++ {
		outputs, genErr := p.generateWithRetry(ctx, request, index)
		result.Outputs = append(result.Outputs, outputs...)
		if genErr != nil {
			result.Failures[index] = genErr.Error()
		}
	}
	if len(result.Failures) == request.N {
		return result, errors.New(lastFailure(result.Failures))
	}
	return result, nil
}

// runParallel 每张图一个 goroutine，按完成顺序收集。
func (p *ImagePool) runParallel(ctx context.Context, request ImageRequest) (PoolResult, error) {
	type outcome struct {
		index   int
		outputs []ImageOutput
		err     error
	}

	results := make(chan outcome, request.N)
	var wg sync.WaitGroup

	for index := 1; index <= request.N; index++ {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			// panic 必须在这里收住。
			//
			// gin.Recovery() 只能拦住 HTTP handler 所在 goroutine 上的 panic；
			// 这是并行生图另起的 goroutine，一个 nil 解引用（上游返回畸形数据时
			// 很常见）会直接终止整个进程 —— 所有在途请求一起死，且日志里只有
			// 一条 goroutine panic。单图路径不受影响（它在 handler goroutine 上）。
			defer func() {
				if recovered := recover(); recovered != nil {
					results <- outcome{
						index: index,
						err:   fmt.Errorf("parallel generation panicked: %v", recovered),
					}
				}
			}()
			// 并行分支必须走同一条重试路径：image_parallel_generation 默认 true，
			// 这里若直接 Acquire+Generate，默认配置下四类预算全部失效。
			outputs, genErr := p.generateWithRetry(ctx, request, index)
			results <- outcome{index: index, outputs: outputs, err: genErr}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	// 按完成顺序收集，但**按索引顺序**输出：
	// 前端依赖 index/total 排序，乱序会让进度条跳。
	collected := make(map[int]outcome, request.N)
	for item := range results {
		collected[item.index] = item
	}

	result := PoolResult{Outputs: make([]ImageOutput, 0, request.N*3), Failures: map[int]string{}}
	emitted := false
	for index := 1; index <= request.N; index++ {
		item, ok := collected[index]
		if !ok {
			continue
		}
		if item.err != nil {
			result.Failures[index] = item.err.Error()
			continue
		}
		if len(item.outputs) == 0 {
			continue
		}
		emitted = true
		result.Outputs = append(result.Outputs, item.outputs...)
	}

	if !emitted {
		lastError := lastFailure(result.Failures)
		if lastError == "" {
			lastError = "no account in the pool could generate images — check account quota and rate-limit status"
		}
		return result, errors.New(ImageStreamErrorMessage(lastError))
	}
	return result, nil
}

// lastFailure 取编号最大的失败原因（与 Python 的 last_error 语义一致：
// 它记录的是循环里最后见到的错误）。
func lastFailure(failures map[int]string) string {
	highest := -1
	message := ""
	for index, text := range failures {
		if index > highest {
			highest = index
			message = text
		}
	}
	return message
}

// CollectImageOutputs 复刻 collect_image_outputs：把 output 序列压成一个响应。
//
// 规则：
//   - created 取第一个非零
//   - 所有 result 的 data 合并
//   - 有 data 就只给 data；没有则把 message / progress 文本作为 message
//   - account_email 取第一个非空，作为内部字段供上层记录
func CollectImageOutputs(outputs []ImageOutput) map[string]any {
	var created int64
	data := make([]any, 0, len(outputs))
	message := ""
	progressParts := make([]string, 0, 4)
	accountEmail := ""

	for _, output := range outputs {
		if created == 0 && output.Created != 0 {
			created = output.Created
		}
		if output.AccountEmail != "" && accountEmail == "" {
			accountEmail = output.AccountEmail
		}
		switch output.Kind {
		case ImageKindProgress:
			if output.Text != "" {
				progressParts = append(progressParts, output.Text)
			}
		case ImageKindMessage:
			message = output.Text
		case ImageKindResult:
			for _, item := range output.Data {
				data = append(data, item)
			}
		}
	}

	// conversationID 必须透出：异步任务的 resume-poll 依赖它继续轮询。
	// 早期实现只产出 created/data/message/_account_email，
	// 导致任务的 conversation_id 永远为空，「继续等待」100% 失效。
	conversationID := ""
	for _, output := range outputs {
		if output.ConversationID != "" {
			conversationID = output.ConversationID
			break
		}
	}

	result := map[string]any{"created": created, "data": data}
	if created == 0 {
		result["created"] = NowUnix()
	}
	if len(data) == 0 {
		text := message
		if text == "" {
			text = joinStrings(progressParts)
		}
		if text != "" {
			result["message"] = text
		}
	}
	if accountEmail != "" {
		result["_account_email"] = accountEmail
	}
	if conversationID != "" {
		result["_conversation_id"] = conversationID
	}
	return result
}

func joinStrings(values []string) string {
	out := ""
	for _, value := range values {
		out += value
	}
	return out
}
