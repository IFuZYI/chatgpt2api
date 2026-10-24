package service

import (
	"github.com/basketikun/chatgpt2api/internal/upstream"
	"time"
)

// RecordInvalidTokenSeen 记一次 token 失效。
//
// 复刻 Python 的 _record_invalid_token_seen（account_service.py:1269-1291）：
// invalid_count +1、last_invalid_at、last_refresh_error、last_refresh_error_at。
// 返回 true 表示调用方应继续走 RemoveInvalidToken。
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
	normalized := normalizeAccount(next)
	if normalized != nil {
		s.accounts[resolved] = normalized
		s.saveAccountsLocked()
	}
	shouldDefer := deferRemoval && s.shouldDeferInvalidTokenLocked(current, now)
	s.mu.Unlock()

	if shouldDefer {
		return false
	}
	return true
}

// RemoveInvalidToken 按配置处理失效 token：删除或标记异常。
//
// 复刻 Python 的 remove_invalid_token（account_service.py:1052-1064）：
//   - auto_remove_invalid_accounts 关闭 → 标记「异常」+ quota 0，保留账号
//   - 开启 → 删除账号，删不掉时退回标记异常
//
// 返回是否真的删除了。
func (s *AccountService) RemoveInvalidToken(accessToken, event string, quiet bool) bool {
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

// shouldDeferInvalidTokenLocked 判断是否应推迟删除（上游抖动保护）。
//
// Python 的实现是 _should_defer_invalid_token；这里用「最近 10 分钟内
// 已被标记过一次失效」作为退避条件，避免一轮刷新里同一账号被反复处理。
func (s *AccountService) shouldDeferInvalidTokenLocked(account Account, now time.Time) bool {
	lastInvalid := stringValue(account["last_invalid_at"])
	if lastInvalid == "" {
		return false
	}
	parsed, err := time.ParseInLocation(accountTimeLayout, lastInvalid, time.Local)
	if err != nil {
		return false
	}
	return now.Sub(parsed) < 10*time.Minute
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
func isTokenInvalidFailure(err error) bool {
	if err == nil {
		return false
	}
	switch upstreamStatusCodeOf(err) {
	case 401, 403:
		return true
	}
	return false
}

// upstreamStatusCodeOf 取上游错误的状态码（薄包装，便于测试替换）。
func upstreamStatusCodeOf(err error) int { return upstream.StatusCodeOf(err) }
