package service

import (
	"fmt"
	"github.com/IFuZYI/chatgpt2api/internal/upstream"
	"strings"
	"time"
)

// invalidTokenDecision 是一次失效观察的处置建议。
//
// 为什么要把「标记异常」和「删除」拆开：原实现只有「推迟/不推迟」一个
// 布尔量，推迟就意味着**什么都不做** —— 首次 401 的账号会带着
// status=正常、quota=0 继续留在调度池里，文本链路照选它、照 401，
// 直到第二次失败才被处理。实测（2026-10-27）号池里就有这种号：
// invalid_count=1、quota=0、last_refresh_error=401、status=正常。
//
// 现在的语义：
//   - MarkAbnormal：立刻标「异常」+ quota 归零（不进调度，但保留账号）
//   - Remove：继续走删除路径（仍受 auto_remove_invalid_accounts 约束）
//
// 两者独立：首次失效 MarkAbnormal=true / Remove=false，
// 即「先摘出调度，但给上游恢复留机会」。
type invalidTokenDecision struct {
	MarkAbnormal bool
	Remove       bool
}

// RecordInvalidTokenSeen 记一次 token 失效。
//
// 复刻 Python 的 _record_invalid_token_seen（account_service.py:1269-1291）：
// invalid_count +1、last_invalid_at、last_refresh_error、last_refresh_error_at。
//
// 返回值语义已变更：**返回 true 表示调用方应继续走 RemoveInvalidToken**
// （即「确认失效、可以删」）。标记异常由本函数自己完成，调用方不必再管。
//
// deferRemoval=true 时若账号在退避窗口内则只记不删（避免上游抖动
// 把整批账号一次性删光）。
func (s *AccountService) RecordInvalidTokenSeen(accessToken, event, reason string, deferRemoval bool) bool {
	now := time.Now()
	reason = trimToDefault(reason, "invalid access token")

	s.mu.Lock()
	resolved := s.resolveTokenLocked(accessToken)
	current, exists := s.accounts[resolved]
	if !exists {
		s.mu.Unlock()
		return true
	}
	next := copyMap(current)
	next["invalid_count"] = intValue(next["invalid_count"]) + 1
	next["last_invalid_at"] = now.Format(accountTimeLayout)
	next["last_refresh_error"] = reason
	next["last_refresh_error_at"] = now.Format(accountTimeLayout)

	decision := invalidTokenDecision{MarkAbnormal: true, Remove: true}
	if deferRemoval {
		decision = s.invalidTokenDecisionLocked(current, now)
	}
	// 标记异常：只有当前不是「禁用」时才覆盖。
	//
	// 为什么保护禁用：禁用是「自动路径已判死」的终态，一个禁用号再被
	// 401 命中不该降级回异常（那会让它重新进入 watcher 的退避重试）。
	if decision.MarkAbnormal && stringValue(current["status"]) != StatusDisabled {
		next["status"] = StatusAbnormal
		next["quota"] = 0
	}
	if account := normalizeAccount(next); account != nil {
		s.accounts[resolved] = account
		s.saveAccountsLocked()
	}
	s.mu.Unlock()

	if decision.MarkAbnormal && !decision.Remove && !deferRemoval {
		// 直接调用（deferRemoval=false）时按老语义：标记后仍走删除判断。
		return true
	}
	return decision.Remove
}

// RemoveInvalidToken 按配置处理失效 token：删除或标记异常。
//
// 复刻 Python 的 remove_invalid_token（account_service.py:1052-1064）：
//   - auto_remove_invalid_accounts 关闭 → 标记「异常」+ quota 0，保留账号
//   - 开启 → 删除账号，删不掉时退回标记异常
//
// 返回是否真的删除了。
//
// **禁用是终态，本函数不得改动它**（两条分支都要挡）。
//
// 回归点（审计发现，Critical）：上层的 RecordInvalidTokenSeen 有"不降级
// 禁用"保护，但它返回的 Remove 标志会驱动调用方进入本函数 —— 而这里原
// 本完全没有禁用判断。触发路径是日常操作：用户点「一键刷新所有账号」，
// 前端会把**全池 token（含禁用行）**都发上来，上游对死 token 返回 401，
// 于是：
//   - auto_remove 关闭 → 禁用被写成「异常」，重新进入 watcher 退避重试
//   - auto_remove 开启 → 禁用账号被**直接删除**，号池数据永久丢失
func (s *AccountService) RemoveInvalidToken(accessToken, event string, quiet bool) bool {
	// 终态保护：禁用账号不参与任何失效处置。
	if s.isDisabled(accessToken) {
		if !quiet && s.logs != nil {
			s.logs.Add(LogTypeAccount, "跳过禁用账号的失效处置", map[string]any{
				"source": event,
				"token":  anonymizeToken(accessToken),
			})
		}
		return false
	}

	if !s.cfg.AutoRemoveInvalidAccounts() {
		s.UpdateAccount(accessToken, Account{"status": StatusAbnormal, "quota": 0}, quiet)
		return false
	}

	deleted := s.DeleteAccounts([]string{accessToken})
	if removed, _ := deleted["removed"].(int); removed > 0 {
		if !quiet && s.logs != nil {
			s.logs.Add(LogTypeAccount, "自动移除异常账号", map[string]any{
				"source": event,
				"token":  anonymizeToken(accessToken),
			})
		}
		return true
	}
	s.UpdateAccount(accessToken, Account{"status": StatusAbnormal, "quota": 0}, quiet)
	return false
}

// isDisabled 判断账号当前是否为「禁用」终态（已解析别名，读锁内取）。
func (s *AccountService) isDisabled(token string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	account, ok := s.accounts[s.resolveTokenLocked(token)]
	if !ok {
		return false
	}
	return stringValue(account["status"]) == StatusDisabled
}

// shouldDeferInvalidTokenLocked 判断是否应推迟删除（上游抖动保护）。
//
// 三道防线，任何一道命中都推迟（即「这次不删」）：
//
//  1. 新号宽限：创建不足 newAccountInvalidGraceSeconds 的账号不删。
//     刚导入的号常因上游会话尚未就绪而首次 401，直接删掉太粗暴。
//  2. 二次确认：invalid_count <= 1 时一律推迟 —— **首次失效绝不删除**。
//     这是最重要的一条。原实现用 last_invalid_at 判断，而首次失效时该字段
//     为空、函数返回 false，于是第一次 401 就直接走删除路径：一轮上游抖动
//     （全部 401）会把整池号删光。
//  3. 短窗去抖：invalidConfirmSeconds 内已确认过一次则推迟，避免同一轮
//     刷新里对同一账号反复处理。
//
// 注意参数是**更新前**的 account（RecordInvalidTokenSeen 传的是 current），
// 所以 invalid_count 读到的是本次 +1 之前的计数。
func (s *AccountService) shouldDeferInvalidTokenLocked(account Account, now time.Time) bool {
	return s.invalidTokenDecisionLocked(account, now).Remove == false
}

// invalidTokenDecisionLocked 把三道防线展开成「标记异常 / 删除」两个决策。
//
// 与 shouldDeferInvalidTokenLocked 的区别就是这里的 MarkAbnormal：
//
//	① 新号宽限   → 不标、不删（刚导入，可能是会话还没就绪）
//	② 短窗去抖   → 不标、不删（同一轮重复命中，已经在处理了）
//	③ 前两次失效 → **标异常**、不删（摘出调度，但留恢复机会）
//	④ 第三次起   → 标异常 + 允许删除（是否真删由 auto_remove 决定）
//
// 注意「前两次」沿用原实现的删除门槛：原代码是
// `invalid_count <= 1 就推迟删除`，而这里拿到的是**更新前**的计数，
// 所以等价于 current >= 2 才允许删除。删除门槛保持不变，
// 新增的只是「标异常」这一步。
func (s *AccountService) invalidTokenDecisionLocked(account Account, now time.Time) invalidTokenDecision {
	// ① 新号宽限
	if created, err := time.ParseInLocation(accountTimeLayout,
		stringValue(account["created_at"]), time.Local); err == nil {
		if now.Sub(created) < time.Duration(newAccountInvalidGraceSeconds)*time.Second {
			return invalidTokenDecision{}
		}
	}

	// ② 短窗去抖（用更新前的 last_invalid_at，即上一次失效的时间）
	if lastInvalid := stringValue(account["last_invalid_at"]); lastInvalid != "" {
		if parsed, err := time.ParseInLocation(accountTimeLayout, lastInvalid, time.Local); err == nil {
			if now.Sub(parsed) < time.Duration(invalidConfirmSeconds)*time.Second {
				return invalidTokenDecision{}
			}
		}
	}

	// ③④ 一律标异常；删除门槛保持原语义（更新前计数 >= 2）
	return invalidTokenDecision{
		MarkAbnormal: true,
		Remove:       intValue(account["invalid_count"]) >= 2,
	}
}

// trimToDefault 去空白，空串时返回兜底值。
func trimToDefault(value, fallback string) string {
	trimmed := stringValue(value)
	if trimmed == "" {
		return fallback
	}
	return trimmed
}

// accountTimeLayout 是 data/ 里时间字段的格式（与 Python 一致）。
const accountTimeLayout = "2006-01-02 15:04:05"

// isTokenInvalidFailure 判断刷新失败是否属于「token 失效」。
//
// 只认 401 / 403：429（限流）与网络错误必须排除 —— 把限流当失效会
// 在一轮刷新里删掉整个号池（Python 只对 InvalidAccessTokenError 走这条路径）。
// isTokenInvalidFailure 判断错误是否代表「token 真的失效了」。
//
// 关键：403 不能无条件算失效。上游的 403 绝大多数是 **Cloudflare 挑战**，
// 而不是死 token —— 仓库自己就记录了这一点（internal/upstream/fingerprint.go:
// `chrome_120 403 challenge ← 不能用`，且完全不带 UA/sec-ch-ua 时连
// chrome_150 也拿 403）。config 里 reset_session_status_codes 默认就是 [403]，
// 即 403 是「换代理/清障」的信号，不是「删号」的信号。
//
// 原来的实现把 401 和 403 一视同仁，后果是一次 TLS/UA/指纹漂移、或一个坏
// 出口 IP，就能把整池健康账号标异常（auto_remove_invalid_accounts 打开时直接删）。
// 这是能在没有任何 token 真正失效的情况下洗掉号池的机制。
//
// 现在的规则：
//   - 401 一律算失效（上游明确说未授权）
//   - 403 **只有**来自 /backend-api/me 时才算失效
//     （那是账号级鉴权端点；conversation/init 的 403 属于会话/挑战层）
func isTokenInvalidFailure(err error) bool {
	if err == nil {
		return false
	}
	switch upstreamStatusCodeOf(err) {
	case 401:
		return true
	case 403:
		// 用错误消息里的路径区分来源（FetchAccountInfo 两个端点各带自己的前缀）。
		msg := err.Error()
		return strings.Contains(msg, "/backend-api/me failed")
	}
	return false
}

// upstreamStatusCodeOf 取上游错误的状态码（薄包装，便于测试替换）。
func upstreamStatusCodeOf(err error) int { return upstream.StatusCodeOf(err) }

// WarnUnsupportedAutoRelogin 在启动时提示 auto_relogin_after_refresh 不生效。
//
// 密码重登链路本身已实现（/api/accounts/re-login → service.ReloginAccounts），
// 但「刷新发现 token 失效后自动触发重登」这条自动路径还没接线：刷新的
// 失败分支只记 invalid_count，不会去调 ReloginAccounts。不提示的话用户会
// 以为开了就会自动恢复异常账号 —— 静默不生效比明确报错更糟。
// WarnUnsupportedAutoRelogin 在开关打开但执行体未注入时给出启动警告。
//
// 语义已变更：auto_relogin_after_refresh 现在是**已实现**的（刷新发现 401/403
// 后自动调 ReloginAccounts）。这里只在「开关开着、但 SetAutoRelogin 没被调用」
// 时警告 —— 那是接线遗漏，不是功能缺失。两者要区分开，否则日志会误导。
func (s *AccountService) WarnUnsupportedAutoRelogin() {
	if s.cfg == nil || !s.cfg.AutoReloginAfterRefresh() {
		return
	}
	s.mu.RLock()
	wired := s.autoReloginFunc() != nil
	s.mu.RUnlock()
	if wired {
		fmt.Println("[account] auto_relogin_after_refresh enabled: 刷新发现失效账号（401/403）时会自动密码重登")
		return
	}
	fmt.Println("[account] auto_relogin_after_refresh is enabled but no password-login " +
		"implementation was injected (SetAutoRelogin); automatic recovery is skipped. " +
		"This is a wiring bug, not a missing feature.")
}
