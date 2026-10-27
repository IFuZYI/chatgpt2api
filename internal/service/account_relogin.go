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
//
// proxyURL 是该账号应当使用的出口代理（账号级 > 全局 > 直连，由
// AccountService.ProxyFor 解析）。**必须由调用方传入**，不能由执行体
// 自己读全局配置 —— 否则同一个账号在「刷新/生图」与「重登」两条路径上
// 会从不同 IP 出去，这本身就是一次 IP 跳变，正是风控要抓的特征。
type PasswordLoginFunc func(ctx context.Context, proxyURL, email, password, totpSecret string) (map[string]string, error)

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

// reloginFailureClass 把重登失败分成三类，决定是否升级为「禁用」。
//
// 策略（产品决定）：**账号无论如何无法恢复就转「禁用」**。
// 包括：
//   - 上游明说账号已停用/删除（封号）
//   - 密码错误（本地凭据已不可用，自动路径改不了它）
//   - 开了 2FA 但本地缺 TOTP 密钥（自动路径永远拿不到那串密钥）
//   - 需要邮箱验证码（自动路径读不了邮箱）
//   - 本地缺 password / email（自动路径无从发起登录）
//
// 这些的共同点是「**再试一万次也不会成功**」。留在「异常」里的唯一后果
// 是每轮白跑一次、把 invalid_count 推到退避上限、让用户以为系统在努力。
//
// 与之相对，环境类失败（出口地区不支持、限流、state 失效、网络）
// 必须保持「异常」继续重试 —— 它们不是账号本身的问题，禁用等于误杀。
type reloginFailureClass int

const (
	// reloginTransient：环境/上游临时问题，保持异常并继续重试。
	reloginTransient reloginFailureClass = iota
	// reloginFatal：确定性不可恢复，自动路径判死 → 禁用（终态）。
	reloginFatal
)

// classifyReloginFailure 按错误码归类。
//
// 白名单式判定：只把**明确**可恢复的环境问题留在 transient，
// 其余（含未知码）按「不可恢复」处理 —— 见下方注释。
func classifyReloginFailure(code string) reloginFailureClass {
	switch code {
	case PasswordLoginAccountDeactivated:
		// 上游明说账号已停用/删除。
		return reloginFatal
	case PasswordLoginInvalidPassword:
		// 密码错：凭据已不可用。本地密码是用户导入的，自动路径改不了它。
		return reloginFatal
	case PasswordLoginNoPassword, PasswordLoginNoEmail:
		// 缺凭据：自动路径无从发起登录，等于永远救不回来。
		return reloginFatal
	case PasswordLoginNeedTOTP, PasswordLoginNeedVerificationCode:
		// 上游要求二次验证，但本地缺 TOTP 密钥 / 需要邮箱验证码。
		// 自动路径拿不到这些，等于换不出新 AT/RT。
		return reloginFatal
	case PasswordLoginUnsupportedRegion,
		PasswordLoginRateLimited,
		PasswordLoginInvalidState,
		PasswordLoginNoAuthCode:
		// 环境/风控：不是账号本身的问题，继续退避重试。
		return reloginTransient
	}
	// 未知码按「不可恢复」处理。
	//
	// 取舍说明：早期实现把未知码当 transient（"保守，不误杀"），
	// 实测后果是账号永远卡在异常、无限重试、且无人知道原因。
	// 现在改为 fatal，但**保留 last_relogin_code 落盘** —— 一旦发现是
	// 良性错误码被误判，可以据此定位并补进上面的 transient 白名单。
	return reloginFatal
}

// ReloginAccounts 对选中账号执行密码重新登录。
//
// 返回值是进度接口的 result 字段：{relogined, skipped, errors[], items[]}。
// 缺 email / password 的账号记 skipped 并在 errors[] 里给出明确错误码，
// 绝不静默跳过 —— 否则用户看到「完成」却不知道哪些号没动。
//
// 手动与自动的差异（progressID 为空即自动路径）：
//
//	手动：只做登录、只报结果，绝不改状态为「禁用」—— 人工点按钮时
//	      不应该因为一次失败就把账号判死。
//	自动：按 classifyReloginFailure 升级 —— 确定性失败标禁用，
//	      缺凭据标 needs_manual 并停止重试。
func (s *AccountService) ReloginAccounts(
	ctx context.Context,
	tokens []string,
	progressID string,
	login PasswordLoginFunc,
) map[string]any {
	unique := DedupeReloginTokens(tokens)
	// 自动路径 = 没有进度对象（watcher / 刷新收尾触发）。
	automatic := progressID == ""

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

		// 自动路径必须跳过「禁用」账号。
		//
		// 回归点：禁用号被刷新探测到 401 时会进入 invalidTokens，进而触发
		// 自动重登；只要密码还能用，ApplyReloginTokens 就会把 status 写成
		// 「正常」—— 禁用被静默解开。applyRefreshedAccount 的禁用保护挡不住
		// 这条路径（它守的是"刷新合并状态"，这里是"重登改写状态"）。
		//
		// 产品要求：禁用后不再自动维护、刷新；解禁通道只能是面板上明确的
		// 「尝试恢复」（手动路径 automatic=false，不受此限制）。
		if automatic && stringValue(account["status"]) == StatusDisabled {
			s.UpdateReloginProgress(progressID, token, "跳过", "账号已禁用，自动路径不重登")
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
			// 缺邮箱 = 自动路径无从发起登录 = 永远救不回来 → 自动路径禁用。
			s.disableReloginUnrecoverable(token, PasswordLoginNoEmail, message, automatic)
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
			// 缺密码同理。
			s.disableReloginUnrecoverable(token, PasswordLoginNoPassword, message, automatic)
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

		// 占住重登权：同一账号不能被两条路径同时密码登录。
		//
		// 跳过时不记 error —— 不是失败，只是「另一轮正在处理它」。
		// 记成错误会让用户看到一条并不存在的故障。
		if !s.beginRelogin(token) {
			s.UpdateReloginProgress(progressID, token, "跳过", "该账号正在重登中")
			continue
		}
		// 出口必须与该账号其它出站一致：账号代理 > 全局代理 > 直连。
		//
		// 在这里解析而不是让执行体自己读配置：只有 AccountService 知道
		// 这个 token 对应哪条账号记录。早期实现让 cmd/server 的工厂闭包
		// 直接用 cfg.Proxy()，导致配了账号代理的号在重登时从全局出口出去。
		proxyURL := s.ProxyFor(token)
		issued, loginErr := func() (map[string]string, error) {
			// 必须用 defer 释放重登权。
			//
			// 回归点：改成手动调用后，login 内部 panic 会跳过 endRelogin，
			// relogining[token] 永久为 true —— 该账号此后**再也无法重登**。
			// 而且这条泄漏是静默的：watcher tick 有 recover，进程不会崩，
			// 只有那个账号悄悄失去恢复能力。
			defer s.endRelogin(token)
			return login(ctx, proxyURL, email, password, totpSecret)
		}()
		if loginErr != nil {
			code := passwordLoginErrorCode(loginErr)
			class := classifyReloginFailure(code)
			// 先取「上一次记录的错误码」——用于未知码的二次确认。
			// 必须在 recordReloginFailure 覆盖它之前读。
			previousCode := s.lastReloginCode(token)
			// 记录失败原因到账号记录：自动路径没有进度对象，
			// 不落盘的话用户完全看不到「为什么没救回来」（原实现如此）。
			s.recordReloginFailure(token, code, loginErr.Error())
			switch {
			case code == PasswordLoginAccountDeactivated:
				// 账号被停用：任何路径都标禁用（这是最确定的终态）。
				s.disableAccount(token, "account_deactivated", loginErr.Error())
				errorsOut = append(errorsOut, map[string]any{
					"token": anonymizeToken(token),
					"error": loginErr.Error(),
				})
				s.UpdateReloginProgress(progressID, token, "禁用", loginErr.Error())
				continue
			case automatic && class == reloginFatal:
				// 自动路径 + 不可恢复 → 禁用。
				//
				// 未知码需要「连续两次看到同一个码」才确认禁用。
				// 理由：已知码（封号、密码错、缺凭据、缺 2FA）语义明确；
				// 而未知码可能只是上游改版或一次短暂故障。不加确认的话，
				// 一轮上游异常就能把整池健康账号打成需要人工解开的终态。
				// 这与本仓库删除路径的「二次确认」是同一套思路。
				if !isKnownReloginFatalCode(code) && previousCode != code {
					errorsOut = append(errorsOut, map[string]any{
						"token": anonymizeToken(token),
						"error": loginErr.Error() + "（未知码首次出现，暂不禁用，等待二次确认）",
					})
					s.UpdateReloginProgress(progressID, token, "异常", loginErr.Error())
					continue
				}
				s.disableAccount(token, code, loginErr.Error())
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

	// 清空失效计数与异常退避锚点。
	//
	// 不重置的后果（实测踩到）：一个被救回来的号 invalid_count 仍停在
	// 7、last_invalid_at 仍是几小时前，于是
	//   - abnormalRetryDueLocked 按 7 次算出 6 小时退避；
	//   - 「首次失效不删」的保护被历史失败永久消耗掉，下次一 401 就可能被删；
	//   - 面板上的「连续失效次数」显示一个早已恢复的账号还在失效。
	// 一次成功的重登意味着之前的失效记录都已失去意义（RFC 9700 §4.14
	// 的轮换语义：新凭据签发表明旧链路已作废）。
	merged["invalid_count"] = 0
	merged["last_invalid_at"] = nil
	merged["last_refresh_error"] = nil
	merged["last_refresh_error_at"] = nil
	// 恢复即清除「需人工」标记：凭据已经齐了。
	merged["needs_manual_reason"] = nil
	merged["needs_manual_at"] = nil
	merged["last_relogin_error"] = nil
	merged["last_relogin_error_at"] = nil
	merged["last_relogin_code"] = nil
	merged["disabled_reason"] = nil
	merged["disabled_at"] = nil
	merged["disabled_detail"] = nil

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

// disableAccount 把账号标为「禁用」并落盘原因。
//
// 禁用是自动路径的终态：不再刷新、不再 keepalive、不再重登，
// 只能由人工在面板上改回。disabled_reason / disabled_at 是排查依据 ——
// 没有它们，用户只看到一个「禁用」，不知道是封号、密码错还是缺凭据。
func (s *AccountService) disableAccount(token, reason, detail string) {
	s.mu.Lock()
	resolved := s.resolveTokenLocked(token)
	current := s.accounts[resolved]
	if current == nil {
		s.mu.Unlock()
		return
	}
	// 已经是禁用就不再改写（保留首次判定的原因与时间）。
	if stringValue(current["status"]) == StatusDisabled {
		s.mu.Unlock()
		return
	}
	next := copyMap(current)
	next["status"] = StatusDisabled
	next["quota"] = 0
	next["disabled_reason"] = reason
	next["disabled_at"] = nowLocal()
	next["disabled_detail"] = truncateText(redactURLCredentials(detail), 300)
	account := normalizeAccount(next)
	if account == nil {
		s.mu.Unlock()
		return
	}
	s.accounts[resolved] = account
	s.saveAccountsLocked()
	s.mu.Unlock()

	if s.logs != nil {
		s.logs.Add(LogTypeAccount, "账号已禁用", map[string]any{
			"token":  anonymizeToken(resolved),
			"reason": reason,
		})
	}
}

// disableReloginUnrecoverable 处理「本地缺凭据」：自动路径下直接禁用。
//
// 策略（产品决定）：账号无论如何无法恢复就转「禁用」。
// 缺 password / email 的号，自动路径连登录都发不出去 —— 等于永远
// 救不回来。留在「异常」里的唯一后果是每轮白跑一次、把 invalid_count
// 推到退避上限，并让用户以为系统在努力。
//
// 手动路径（automatic=false）不动状态：用户点按钮时可能正准备补凭据，
// 此时把号判死会切断唯一的修复路径。手动路径仍会记 last_relogin_code，
// 面板上能看到原因。
func (s *AccountService) disableReloginUnrecoverable(token, reason, detail string, automatic bool) {
	if !automatic {
		return
	}
	s.disableAccount(token, reason, detail)
}

// recordReloginFailure 把重登失败落到账号记录上。
//
// 原实现只写进度对象，而自动路径的 progressID 为空 → UpdateReloginProgress
// 直接 return → 失败原因**完全不落盘**。用户只能看到账号卡在异常，
// 没有任何线索知道是密码错、地区不支持还是网络问题。
func (s *AccountService) recordReloginFailure(token, code, detail string) {
	s.mu.Lock()
	resolved := s.resolveTokenLocked(token)
	current := s.accounts[resolved]
	if current == nil {
		s.mu.Unlock()
		return
	}
	next := copyMap(current)
	// 脱敏：上游/客户端错误文本可能带回代理 URL 的账号密码。
	next["last_relogin_error"] = truncateText(redactURLCredentials(detail), 300)
	next["last_relogin_error_at"] = nowLocal()
	next["last_relogin_code"] = code
	account := normalizeAccount(next)
	if account == nil {
		s.mu.Unlock()
		return
	}
	s.accounts[resolved] = account
	s.saveAccountsLocked()
	s.mu.Unlock()
}

// knownReloginFatalCodes 是语义明确、可以立即禁用的错误码。
//
// 不在这里的码一律走「二次确认」——包括所有未知码。这个白名单是
// 有意为之的窄集合：每加一个码都要确认它确实表示「自动路径永远救不回来」。
var knownReloginFatalCodes = map[string]bool{
	PasswordLoginAccountDeactivated:   true,
	PasswordLoginInvalidPassword:      true,
	PasswordLoginNoPassword:           true,
	PasswordLoginNoEmail:              true,
	PasswordLoginNeedTOTP:             true,
	PasswordLoginNeedVerificationCode: true,
}

// isKnownReloginFatalCode 判断错误码是否属于"语义明确、可立即禁用"的集合。
func isKnownReloginFatalCode(code string) bool {
	return knownReloginFatalCodes[code]
}

// lastReloginCode 读账号上一次记录的重登错误码（用于未知码二次确认）。
func (s *AccountService) lastReloginCode(token string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	account, ok := s.accounts[s.resolveTokenLocked(token)]
	if !ok {
		return ""
	}
	return stringValue(account["last_relogin_code"])
}
