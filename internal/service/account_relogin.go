package service

import (
	"context"
	"errors"
	"strings"
)

// 本文件是 /api/accounts/re-login 的业务层：遍历选中账号、调登录执行体、
// 把结果写回号池并推进进度。API 层只负责注入执行体与启停后台任务。
//
// Python 版每个账号起一个线程后立刻返回（进度靠线程自己收尾），
// 这里串行处理：进度语义（relogined 计数 + errors[] + results[]）保持一致，
// 但完成时机确定，不会出现「线程还在跑、进度已 done」的竞态。

// PasswordLoginFunc 是单个账号的密码登录执行体（生产实现见 LoginWithPassword）。
type PasswordLoginFunc func(ctx context.Context, email, password, totpSecret string) (map[string]string, error)

// reloginAccountFields 从账号里取登录凭据。totp_secret 允许缺省（没开两步验证的号）。
func reloginAccountFields(account Account) (email, password, totpSecret string) {
	return strings.TrimSpace(stringValue(account["email"])),
		stringValue(account["password"]),
		strings.TrimSpace(stringValue(account["totp_secret"]))
}

// passwordLoginErrorCode 取出 *PasswordLoginError 的错误码；其它错误返回空串。
func passwordLoginErrorCode(err error) string {
	var loginErr *PasswordLoginError
	if errors.As(err, &loginErr) {
		return loginErr.Code
	}
	return ""
}

// DedupeReloginTokens 去掉空值与重复 token，保持输入顺序。
//
// 暴露出来的理由：API 层要在**返回 progress_id 之前**就建好进度对象
// （否则前端首个轮询会 404），而 total 必须与真正会被处理的账号数一致。
func DedupeReloginTokens(tokens []string) []string {
	unique := make([]string, 0, len(tokens))
	seen := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		trimmed := strings.TrimSpace(token)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		unique = append(unique, trimmed)
	}
	return unique
}

// ReloginAccounts 对选中账号执行密码重新登录。
//
// 返回值是进度接口的 result 字段：{relogined, skipped, errors[], items[]}。
// 缺 email / password 的账号记 skipped 并在 errors[] 里给出明确错误码，
// 绝不静默跳过 —— 否则用户看到「完成」却不知道哪些号没动。
func (s *AccountService) ReloginAccounts(
	ctx context.Context,
	tokens []string,
	progressID string,
	login PasswordLoginFunc,
) map[string]any {
	unique := DedupeReloginTokens(tokens)

	// 调用方（API 层）通常已经建好进度，这里只兜底：直接调用本函数时
	// 也要留下 done=true 的进度对象，否则前端轮询到 404、进度条永远转。
	if progressID != "" {
		if s.GetReloginProgress(progressID) == nil {
			s.InitReloginProgress(progressID, len(unique))
		}
	}
	if len(unique) == 0 {
		// 同 RefreshAccounts：进度结果不放 items —— 它会被
		// FinishReloginProgress 存进 Progress.Result 并由进度接口原样返回，
		// 等于泄漏全池凭据。
		result := map[string]any{
			"relogined": 0,
			"skipped":   0,
			"errors":    []any{},
		}
		if progressID != "" {
			s.FinishReloginProgress(progressID, result, "")
		}
		return result
	}

	relogined := 0
	skipped := 0
	errorsOut := make([]any, 0)

	for _, token := range unique {
		account := s.GetAccount(token)
		if account == nil {
			errorsOut = append(errorsOut, map[string]any{
				"token": anonymizeToken(token),
				"error": "账号不存在",
			})
			s.UpdateReloginProgress(progressID, token, "跳过", "账号不存在")
			continue
		}

		email, password, totpSecret := reloginAccountFields(account)
		if email == "" {
			skipped++
			message := PasswordLoginNoEmail + ": 账号缺少邮箱，无法发起密码登录"
			errorsOut = append(errorsOut, map[string]any{
				"token": anonymizeToken(token),
				"error": message,
			})
			s.UpdateReloginProgress(progressID, token, "跳过", message)
			continue
		}
		if strings.TrimSpace(password) == "" {
			skipped++
			message := PasswordLoginNoPassword + ": 账号缺少密码，无法发起密码登录"
			errorsOut = append(errorsOut, map[string]any{
				"token": anonymizeToken(token),
				"error": message,
			})
			s.UpdateReloginProgress(progressID, token, "跳过", message)
			continue
		}

		if login == nil {
			message := "password_login_not_configured: 密码登录执行体未注入"
			errorsOut = append(errorsOut, map[string]any{
				"token": anonymizeToken(token),
				"error": message,
			})
			s.UpdateReloginProgress(progressID, token, "异常", message)
			continue
		}

		issued, loginErr := login(ctx, email, password, totpSecret)
		if loginErr != nil {
			code := passwordLoginErrorCode(loginErr)
			if code == PasswordLoginAccountDeactivated {
				// 账号被停用：标禁用并清额度（与 Python 一致）。
				s.UpdateAccount(token, Account{"status": StatusDisabled, "quota": 0}, true)
				errorsOut = append(errorsOut, map[string]any{
					"token": anonymizeToken(token),
					"error": loginErr.Error(),
				})
				s.UpdateReloginProgress(progressID, token, "禁用", loginErr.Error())
				continue
			}
			errorsOut = append(errorsOut, map[string]any{
				"token": anonymizeToken(token),
				"error": loginErr.Error(),
			})
			s.UpdateReloginProgress(progressID, token, "异常", loginErr.Error())
			continue
		}

		if !s.ApplyReloginTokens(token, issued) {
			message := "account_missing_during_relogin: 登录成功但账号已不在号池（可能被并发删除）"
			errorsOut = append(errorsOut, map[string]any{
				"token": anonymizeToken(token),
				"error": message,
			})
			s.UpdateReloginProgress(progressID, token, "异常", message)
			continue
		}
		relogined++
		s.UpdateReloginProgress(progressID, token, "成功", "")
	}

	// 同样不放 items（见上）。
	result := map[string]any{
		"relogined": relogined,
		"skipped":   skipped,
		"errors":    errorsOut,
	}
	if progressID != "" {
		s.FinishReloginProgress(progressID, result, "")
	}
	return result
}

// ApplyReloginTokens 把密码登录拿到的新 token 写回号池。
//
// 与 applyRefreshedAccount 的差异：这里必须**同时**更新 refresh_token / id_token
// 与 source_type —— 密码登录是账号级的重新授权，不是普通刷新。
// access_token 可能轮换，旧 token 记进别名表，让前端手里的旧值仍能解析。
// 返回 false 表示号池里已经找不到这个账号。
func (s *AccountService) ApplyReloginTokens(oldToken string, issued map[string]string) bool {
	newToken := strings.TrimSpace(issued["access_token"])
	if newToken == "" {
		return false
	}
	refreshToken := strings.TrimSpace(issued["refresh_token"])
	idToken := strings.TrimSpace(issued["id_token"])

	s.mu.Lock()
	defer s.mu.Unlock()

	resolved := s.resolveTokenLocked(oldToken)
	current := s.accounts[resolved]
	if current == nil {
		return false
	}

	merged := copyMap(current)
	merged["access_token"] = newToken
	if refreshToken != "" {
		merged["refresh_token"] = refreshToken
	}
	if idToken != "" {
		merged["id_token"] = idToken
	}
	merged["source_type"] = SourcePassword
	merged["status"] = StatusNormal

	// 重登成功即重置 keepalive 状态。
	//
	// 为什么必须重置：重登刚拿到一个全新的 RT，它应当是 keepalive 循环的新起点。
	// 不重置的话锚点还停在旧值上，下一轮 tick 会立刻再挑中这个账号 ——
	// 刚花完一次密码登录又被拉去换 AT，白白多一次请求。
	//
	// 同时清掉上一次的失败记录：留着会让面板显示过时的错误，
	// 并让 refreshKeepaliveErrorBackoff（6 小时）错误地继续生效。
	merged["last_token_refresh_at"] = nowLocal()
	merged["last_token_refresh_error"] = nil
	merged["last_token_refresh_error_at"] = nil

	account := normalizeAccount(merged)
	if account == nil {
		return false
	}

	if resolved != newToken {
		delete(s.accounts, resolved)
		s.aliases[resolved] = newToken
		s.slotsMu.Lock()
		delete(s.slots, resolved)
		s.slotsMu.Unlock()
		s.inflight.Delete(resolved)
	}
	s.accounts[newToken] = account
	s.saveAccountsLocked()
	return true
}
