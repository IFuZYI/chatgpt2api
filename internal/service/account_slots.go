package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// ErrNoImageQuota 与 Python 的 "no available image quota" 对应。
// api 层据此返回 429 insufficient_quota。
var ErrNoImageQuota = errors.New("no available image quota")

// SlotOptions 是候选筛选条件，对应 Python 的 plan_type / source_type / plan_types。
type SlotOptions struct {
	PlanType   string
	SourceType string
	PlanTypes  []string
}

func (o SlotOptions) label() string {
	parts := make([]string, 0, 2)
	if o.PlanType != "" {
		parts = append(parts, o.PlanType)
	}
	if o.SourceType != "" {
		parts = append(parts, o.SourceType)
	}
	return strings.Join(parts, " ")
}

// noQuotaError 复刻 Python 的措辞：
//
//	f"no available {plan_type or source_type or ''} image quota"
//
// 带前缀时前缀与 "image quota" 之间只有一个空格。
func noQuotaError(opts SlotOptions, tried int) error {
	label := opts.label()
	var message string
	if label != "" {
		message = fmt.Sprintf("no available %s image quota", label)
	} else {
		message = "no available image quota"
	}
	if tried > 0 {
		message = fmt.Sprintf("%s (tried %d tokens)", message, tried)
	}
	return errors.New(message)
}

func (s *AccountService) inflightFor(token string) *atomic.Int64 {
	if value, ok := s.inflight.Load(token); ok {
		return value.(*atomic.Int64)
	}
	counter := &atomic.Int64{}
	actual, _ := s.inflight.LoadOrStore(token, counter)
	return actual.(*atomic.Int64)
}

// slotFor 返回（必要时创建）某账号的信号量。容量取当前配置，
// 配置热更新时新建的信号量会拿到新容量，已存在的沿用旧容量直到账号删除。
func (s *AccountService) slotFor(token string) chan struct{} {
	s.slotsMu.Lock()
	defer s.slotsMu.Unlock()
	if existing, ok := s.slots[token]; ok {
		return existing
	}
	capacity := s.cfg.ImageAccountConcurrency()
	created := make(chan struct{}, capacity)
	s.slots[token] = created
	return created
}

// readyCandidateTokensLocked 返回所有「就绪」账号（状态正常、有额度、匹配筛选）。
func (s *AccountService) readyCandidateTokensLocked(excluded map[string]bool, opts SlotOptions) []string {
	out := make([]string, 0, len(s.accounts))
	for token, account := range s.accounts {
		if excluded[token] {
			continue
		}
		if !s.isImageAccountAvailable(account) {
			continue
		}
		if !accountMatchesPlanType(account, opts.PlanType) {
			continue
		}
		if !accountMatchesAnyPlanType(account, opts.PlanTypes) {
			continue
		}
		if !accountMatchesSourceType(account, opts.SourceType) {
			continue
		}
		out = append(out, token)
	}
	return out
}

// acquireSlot 取一个账号槽位。语义与 Python 的 _acquire_next_candidate_token 对齐：
//   - 没有任何就绪账号 → 立即返回错误
//   - 有就绪账号但全部占满 → 阻塞等待（1 秒轮询对齐 Python 的 wait(timeout=1.0)）
//   - ctx 取消 → 返回 ctx.Err()
func (s *AccountService) acquireSlot(ctx context.Context, excluded map[string]bool, opts SlotOptions) (string, error) {
	if excluded == nil {
		excluded = map[string]bool{}
	}
	for {
		// 读锁即可：readyCandidateTokensLocked 只遍历 s.accounts 读字段，
		// 不做任何写入。原来用写锁 Lock() 会让所有并发取号的请求串行化 ——
		// 实测 300 账号下并行比串行还慢（80.3 µs vs 60.5 µs），
		// 正是写锁争用的典型表现。
		s.mu.RLock()
		ready := s.readyCandidateTokensLocked(excluded, opts)
		if len(ready) == 0 {
			s.mu.RUnlock()
			return "", noQuotaError(opts, 0)
		}
		// 轮询游标，与 Python 的 self._index 一致：在就绪集合上均匀轮转。
		//
		// 用一次 make + 两次 copy 构造轮转视图，而不是
		// `append(append([]string{}, ready[start:]...), ready[:start]...)` ——
		// 后者会分配两个 slice（内层再扩容一次）。300 账号时实测
		// 5 allocs / 12.5 KB per acquire，这是图片链路的主要分配来源。
		start := int(s.cursor.Add(1)-1) % len(ready)
		ordered := make([]string, len(ready))
		n := copy(ordered, ready[start:])
		copy(ordered[n:], ready[:start])
		s.mu.RUnlock()

		for _, token := range ordered {
			slot := s.slotFor(token)
			select {
			case slot <- struct{}{}:
				s.inflightFor(token).Add(1)
				return token, nil
			default:
				continue
			}
		}

		// 全部占满：等一会儿再试，同时尊重 ctx。
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timeAfterSecond():
		}
	}
}

// AcquireImageSlot 是 acquireSlot 的导出包装，供 api 层直接取槽位。
func (s *AccountService) AcquireImageSlot(ctx context.Context, opts SlotOptions) (string, error) {
	return s.acquireSlot(ctx, map[string]bool{}, opts)
}

// ReleaseImageSlot 释放槽位。必须成对调用（handler 里用 defer）。
func (s *AccountService) ReleaseImageSlot(token string) {
	if token == "" {
		return
	}
	s.mu.RLock()
	resolved := s.resolveTokenLocked(token)
	s.mu.RUnlock()
	if resolved == "" {
		return
	}
	slot, ok := func() (chan struct{}, bool) {
		s.slotsMu.Lock()
		defer s.slotsMu.Unlock()
		existing, found := s.slots[resolved]
		return existing, found
	}()
	if ok {
		select {
		case <-slot:
		default:
			// 未占用却来释放：忽略，不 panic（Python 版这里会减到 0 以下再被 pop）。
		}
	}
	if counter, loaded := s.inflight.Load(resolved); loaded {
		value := counter.(*atomic.Int64)
		if value.Add(-1) <= 0 {
			s.inflight.Delete(resolved)
		}
	}
}

// GetAvailableAccessToken 复刻 Python：取槽位后做一次远程校验，失败则换号。
// verify 由上层注入（upstream 包实现），避免 service 依赖 upstream。
//
// 未接线：当前生产路径走 image_pool 的 Acquire + 池内重试，不经过这里。
// 保留是因为它承载了 Python 的「校验失败就换号」语义（含 20 次上限与
// 槽位释放），将来若把远程校验移到取号阶段可以直接接上。
// 改动这里请先补测试 —— 目前没有测试覆盖，也没有调用者。
func (s *AccountService) GetAvailableAccessToken(
	ctx context.Context,
	opts SlotOptions,
	verify func(ctx context.Context, token string) (Account, error),
) (string, error) {
	const maxAttempts = 20
	attempted := map[string]bool{}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		token, err := s.acquireSlot(ctx, attempted, opts)
		if err != nil {
			if errors.Is(err, ErrNoImageQuota) && attempt > 0 {
				return "", noQuotaError(opts, len(attempted))
			}
			return "", err
		}
		attempted[token] = true

		if verify == nil {
			return token, nil
		}
		account, verifyErr := verify(ctx, token)
		if verifyErr != nil {
			s.ReleaseImageSlot(token)
			continue
		}
		resolved := stringValue(account["access_token"])
		if resolved != "" && resolved != token {
			attempted[resolved] = true
		}
		if s.isImageAccountAvailable(account) &&
			accountMatchesPlanType(account, opts.PlanType) &&
			accountMatchesAnyPlanType(account, opts.PlanTypes) &&
			accountMatchesSourceType(account, opts.SourceType) {
			if resolved == "" {
				return token, nil
			}
			return resolved, nil
		}
		s.ReleaseImageSlot(token)
	}
	return "", noQuotaError(opts, len(attempted))
}

// GetTextAccessToken 复刻 Python：文本链路不占图片槽位，只按模型路由筛账号。
// routeAccountTypes 为 nil 表示未命中路由（等价 Python 的 route is None）。
func (s *AccountService) GetTextAccessToken(excluded map[string]bool, routeAccountTypes map[string]bool, allowAnonymous bool) (string, error) {
	if excluded == nil {
		excluded = map[string]bool{}
	}
	s.mu.RLock()
	// 复用候选切片，避免每个请求都分配一个 len(accounts) 的 slice。
	//
	// 这是每个 /v1/chat/completions 都会走的路径：300 账号时实测
	// 43.8 µs / 5.4 KB 分配。高 QPS 下这些短命对象是 GC 的主要压力源。
	// 用 sync.Pool 借还，稳态下分配次数降到 0。
	candidates, _ := s.textCandidateBuf.Get().(*[]string)
	if candidates == nil {
		buf := make([]string, 0, len(s.accounts))
		candidates = &buf
	}
	*candidates = (*candidates)[:0]
	for token, account := range s.accounts {
		// 白名单：只有「正常」与「限流」可调度，未知状态一律不可调度。
		// 黑名单写法会让导入的英文状态（banned/expired…）被当作可用号。
		if !isSchedulableStatus(stringValue(account["status"])) {
			continue
		}
		if excluded[token] {
			continue
		}
		if routeAccountTypes != nil {
			if !routeAccountTypes[normalizeAccountType(account["type"])] {
				continue
			}
		}
		*candidates = append(*candidates, token)
	}
	n := len(*candidates)
	var picked string
	if n > 0 {
		picked = (*candidates)[int(s.cursor.Add(1)-1)%n]
	}
	s.mu.RUnlock()

	// 归还缓冲前**显式清零底层数组**。
	//
	// 只写 `*candidates = (*candidates)[:0]` 是不够的：切片长度归零
	// 但底层数组仍持有那些字符串的引用，GC 无法回收 —— 池里会一直钉住
	// 上一轮全池的 token 字符串，等于用一个「省分配」的优化换来一处
	// 常驻内存泄漏（实测：清零前池中的缓冲仍能读到 "secret-token"）。
	clear(*candidates)
	*candidates = (*candidates)[:0]
	s.textCandidateBuf.Put(candidates)

	if n == 0 {
		if routeAccountTypes == nil || allowAnonymous {
			return "", nil
		}
		return "", errors.New("model is not available to any active account")
	}
	return picked, nil
}

// MarkTextUsed 更新 last_used_at 并落盘（Python 每次都写）。
func (s *AccountService) MarkTextUsed(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resolved := s.resolveTokenLocked(token)
	current := s.accounts[resolved]
	if current == nil {
		return
	}
	next := copyMap(current)
	next["last_used_at"] = localTimestamp()
	normalized := normalizeAccount(next)
	if normalized == nil {
		return
	}
	s.accounts[resolved] = normalized
	s.saveAccountsLocked()
}

// MarkImageResult 复刻 Python：先释放槽位，再记账。
// 成功扣 1 额度，额度归零转「限流」；失败只加 fail。
func (s *AccountService) MarkImageResult(token string, success bool) Account {
	if token == "" {
		return nil
	}
	s.ReleaseImageSlot(token)

	s.mu.Lock()
	defer s.mu.Unlock()
	resolved := s.resolveTokenLocked(token)
	current := s.accounts[resolved]
	if current == nil {
		return nil
	}
	next := copyMap(current)
	next["last_used_at"] = localTimestamp()
	// statusTransitionsAllowed 决定这次调用能不能改 status。
	//
	// 关键：图片任务只在「正常 ↔ 限流」这一对状态之间做转换。
	// 「异常」与「禁用」是账号可用性的判定，不是用量状态 —— 一个正在
	// 处理图片的账号可能同时被判为失效（上游 401）或封号，此时图片任务
	// 成功返回、quota 归零，若无条件写「限流」，就把刚判死的账号重新
	// 激活了（文本链路接受限流号）。实测确认过这条覆写。
	//
	// 排除法而不是白名单：这里检查的是「当前状态是否属于可被用量改写的
	// 状态」，未知状态按不可改写处理，避免把一个陌生状态意外激活。
	statusTransitionsAllowed := func(status string) bool {
		return status == StatusNormal || status == StatusLimited
	}
	if success {
		next["success"] = intValue(next["success"]) + 1
		next["quota"] = maxInt(intValue(next["quota"])-1, 0)
		if statusTransitionsAllowed(stringValue(current["status"])) {
			if intValue(next["quota"]) == 0 {
				next["status"] = StatusLimited
				if stringValue(next["restore_at"]) == "" {
					next["restore_at"] = nil
				}
			} else if stringValue(next["status"]) == StatusLimited {
				next["status"] = StatusNormal
			}
		}
	} else {
		next["fail"] = intValue(next["fail"]) + 1
	}
	normalized := normalizeAccount(next)
	if normalized == nil {
		return nil
	}
	if stringValue(normalized["status"]) == StatusLimited && s.cfg.AutoRemoveRateLimitedAccounts() {
		delete(s.accounts, resolved)
		s.dropSlotLocked(resolved)
		s.saveAccountsLocked()
		s.logs.Add(LogTypeAccount, "自动移除限流账号", map[string]any{
			"token": anonymizeToken(resolved),
		})
		return nil
	}
	s.accounts[resolved] = normalized
	s.saveAccountsLocked()
	return copyMap(normalized)
}

// dropSlotLocked 在已持 mu 的情况下清理槽位表（slotsMu 是独立锁，顺序固定 mu→slotsMu）。
func (s *AccountService) dropSlotLocked(token string) {
	delete(s.aliases, token)
	for old, target := range s.aliases {
		if target == token {
			delete(s.aliases, old)
		}
	}
	s.slotsMu.Lock()
	delete(s.slots, token)
	s.slotsMu.Unlock()
	s.inflight.Delete(token)
}
