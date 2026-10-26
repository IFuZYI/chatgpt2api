package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// prepareAccountPayload 复刻 Python 的 _prepare_account_payload：
// CPA/Codex 导出文件里的 type=codex 是导出格式而非套餐类型，要搬到 export_type。
func prepareAccountPayload(item map[string]any) map[string]any {
	if item == nil {
		return nil
	}
	accessToken := strings.TrimSpace(stringValue(item["access_token"]))
	if accessToken == "" {
		accessToken = strings.TrimSpace(stringValue(item["accessToken"]))
	}
	if accessToken == "" {
		return nil
	}
	payload := copyMap(item)
	delete(payload, "accessToken")
	payload["access_token"] = accessToken
	if strings.ToLower(strings.TrimSpace(stringValue(payload["type"]))) == "codex" {
		payload["export_type"] = "codex"
		payload["source_type"] = SourceCodex
		delete(payload, "type")
	}
	if strings.ToLower(strings.TrimSpace(stringValue(payload["export_type"]))) == "codex" {
		payload["source_type"] = SourceCodex
	}
	if stringValue(payload["plan_type"]) != "" && stringValue(payload["type"]) == "" {
		payload["type"] = strings.TrimSpace(stringValue(payload["plan_type"]))
	}
	return payload
}

// AddAccountItems 接受完整账号对象（CPA/sub2api/OAuth 导入路径）。
func (s *AccountService) AddAccountItems(items []map[string]any, extraTokens []string) map[string]any {
	payloads := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if payload := prepareAccountPayload(item); payload != nil {
			payloads = append(payloads, payload)
		}
	}
	// 纯 token 形式（extraTokens）走 web 来源。
	//
	// 必须排除已经出现在 items 里的 token —— 否则同一个 token 会先以
	// items 里的真实来源（codex / cpa / sub2api）入池，再被这里的
	// {"source_type":"web"} 覆盖（addAccountPayloads 后写覆盖）。
	// Python api/accounts.py:226 用 payload_token_set 做同样的排除。
	//
	// 后果（实测）：codex 账号被标成 web → /v1/models 不投放 codex 图模、
	// codex 定向调度选不到号。
	seen := make(map[string]bool, len(payloads))
	for _, payload := range payloads {
		if token := strings.TrimSpace(stringValue(payload["access_token"])); token != "" {
			seen[token] = true
		}
	}
	for _, token := range extraTokens {
		trimmed := strings.TrimSpace(token)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		payloads = append(payloads, map[string]any{
			"access_token": trimmed,
			"source_type":  SourceWeb,
		})
	}
	result := s.addAccountPayloads(payloads)
	// Python 的 POST /api/accounts 在新增后立刻刷新一次，返回 refreshed/errors/items。
	return result
}

// AddAccounts 是纯 token 入口。
func (s *AccountService) AddAccounts(tokens []string, sourceType string) map[string]any {
	if sourceType == "" {
		sourceType = SourceWeb
	}
	seen := map[string]bool{}
	payloads := make([]map[string]any, 0, len(tokens))
	for _, token := range tokens {
		trimmed := strings.TrimSpace(token)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		payloads = append(payloads, map[string]any{
			"access_token": trimmed,
			"source_type":  normalizeSourceType(sourceType),
		})
	}
	if len(payloads) == 0 {
		return map[string]any{"added": 0, "skipped": 0, "items": s.ListAccounts()}
	}
	return s.addAccountPayloads(payloads)
}

func (s *AccountService) addAccountPayloads(payloads []map[string]any) map[string]any {
	deduped := map[string]map[string]any{}
	order := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		if payload == nil {
			continue
		}
		accessToken := strings.TrimSpace(stringValue(payload["access_token"]))
		if accessToken == "" {
			continue
		}
		merged, seen := deduped[accessToken]
		if !seen {
			merged = map[string]any{}
			order = append(order, accessToken)
		}
		for key, value := range payload {
			merged[key] = value
		}
		merged["access_token"] = accessToken
		deduped[accessToken] = merged
	}
	if len(deduped) == 0 {
		return map[string]any{"added": 0, "skipped": 0, "items": s.ListAccounts()}
	}

	s.mu.Lock()
	// 快照「本轮之前就存在」的账号 —— 它们的 account_id 已经在磁盘上
	// （或已在本进程的先前一轮里分配过），消歧时必须优先保留：
	// 它们可能已被外部系统引用（日志、书签、注册机的库）。
	//
	// 必须在插入循环**之前**快照：循环里新账号会被 normalizeAccount
	// 填上刚派生的 id，与旧账号分不出来。
	preExisting := make(map[string]bool, len(s.accounts))
	for token, account := range s.accounts {
		if strings.TrimSpace(stringValue(account["account_id"])) != "" {
			preExisting[token] = true
		}
	}
	added, skipped := 0, 0
	for _, accessToken := range order {
		payload := deduped[accessToken]
		current := s.accounts[accessToken]
		if current == nil {
			added++
			s.cumulativeMu.Lock()
			s.cumulativeTotal++
			s.cumulativeMu.Unlock()
			s.saveCumulativeTotal()
			// nowLocal 而不是 nowString：账号所有时间字段必须同一种时区，
			// 否则 parseAccountTime 无法用统一规则解析。
			//
			// 这是 created_at 的**真实**写入点（AddAccounts/AddAccountItems），
			// 比 normalizeAccount 里的补空分支更早生效 —— 只改后者等于没改：
			// 新账号的 created_at 在这里就被写成 UTC，normalizeAccount 只填空值。
			current = map[string]any{"created_at": nowLocal()}
		} else {
			skipped++
		}
		incoming := copyMap(payload)
		if stringValue(incoming["created_at"]) == "" {
			delete(incoming, "created_at")
		}
		merged := copyMap(current)
		for key, value := range incoming {
			merged[key] = value
		}
		merged["access_token"] = accessToken
		if stringValue(incoming["type"]) == "" && stringValue(current["type"]) == "" {
			merged["type"] = "free"
		} else if stringValue(incoming["type"]) == "" {
			merged["type"] = current["type"]
		}
		if account := normalizeAccount(merged); account != nil {
			s.accounts[accessToken] = account
		}
	}
	// 新导入的账号可能与已有账号共享 user_id/email（重复导入、同一邮箱多账号），
	// 那样会派生出相同的 account_id —— 必须消歧，否则按 ID 查询会解析到
	// 不确定的账号（Go 的 map 迭代顺序随机）。
	//
	// 这里传 nil 而不是「所有已有账号」：本轮的账号 id 都是 normalizeAccount
	// 刚派生的，尚未落盘，不享有「持久化优先」—— 否则新账号会把自己刚派生的
	// id 占住，冲突的另一个反而拿不到消歧。
	//
	disambiguateAccountIDs(s.accounts, preExisting)
	s.saveAccountsLocked()
	items := s.listAccountsLocked()
	s.mu.Unlock()

	s.logs.Add(LogTypeAccount,
		"新增 "+itoa(added)+" 个账号，跳过 "+itoa(skipped)+" 个",
		map[string]any{"added": added, "skipped": skipped})
	return map[string]any{"added": added, "skipped": skipped, "items": items}
}

func (s *AccountService) listAccountsLocked() []Account {
	items := make([]Account, 0, len(s.accounts))
	for _, account := range s.accounts {
		items = append(items, copyMap(account))
	}
	for _, account := range items {
		token := stringValue(account["access_token"])
		account["image_inflight"] = int(s.inflightFor(token).Load())
	}
	sort.SliceStable(items, func(i, j int) bool {
		left := stringValue(items[i]["created_at"])
		right := stringValue(items[j]["created_at"])
		if left != right {
			return left < right
		}
		return stringValue(items[i]["access_token"]) < stringValue(items[j]["access_token"])
	})
	return items
}

// DeleteAccounts 复刻 Python：先按别名解析 token，再删。
func (s *AccountService) DeleteAccounts(tokens []string) map[string]any {
	targets := map[string]bool{}
	for _, token := range tokens {
		if trimmed := strings.TrimSpace(token); trimmed != "" {
			targets[trimmed] = true
		}
	}
	if len(targets) == 0 {
		return map[string]any{"removed": 0, "items": s.ListAccounts()}
	}

	s.mu.Lock()
	resolved := map[string]bool{}
	for token := range targets {
		resolved[s.resolveTokenLocked(token)] = true
	}
	removed := 0
	for token := range resolved {
		if _, ok := s.accounts[token]; ok {
			delete(s.accounts, token)
			removed++
		}
		s.dropSlotLocked(token)
	}
	if removed > 0 {
		s.saveAccountsLocked()
	}
	items := s.listAccountsLocked()
	s.mu.Unlock()

	if removed > 0 {
		s.logs.Add(LogTypeAccount, "删除 "+itoa(removed)+" 个账号", map[string]any{"removed": removed})
	}
	return map[string]any{"removed": removed, "items": items}
}

// UpdateAccount 复刻 Python，包括「限流且开启自动移除」时直接删除。
func (s *AccountService) UpdateAccount(token string, updates map[string]any, quiet bool) Account {
	if token == "" {
		return nil
	}
	s.mu.Lock()
	resolved := s.resolveTokenLocked(token)
	current := s.accounts[resolved]
	if current == nil {
		s.mu.Unlock()
		return nil
	}
	merged := copyMap(current)
	for key, value := range updates {
		merged[key] = value
	}
	merged["access_token"] = resolved
	account := normalizeAccount(merged)
	if account == nil {
		s.mu.Unlock()
		return nil
	}
	if stringValue(account["status"]) == StatusLimited && s.cfg.AutoRemoveRateLimitedAccounts() {
		delete(s.accounts, resolved)
		s.dropSlotLocked(resolved)
		s.saveAccountsLocked()
		s.mu.Unlock()
		s.logs.Add(LogTypeAccount, "自动移除限流账号", map[string]any{
			"token": anonymizeToken(resolved),
		})
		return nil
	}
	s.accounts[resolved] = account
	s.saveAccountsLocked()
	result := copyMap(account)
	s.mu.Unlock()

	if !quiet {
		s.logs.Add(LogTypeAccount, "更新账号", map[string]any{
			"token":  anonymizeToken(resolved),
			"status": stringValue(account["status"]),
		})
	}
	return result
}

// ---- 刷新 ----

// RefreshVerifier 由 upstream 层注入：给一个 token，返回最新账号信息。
// 返回的 account 里 access_token 可能与入参不同（token 轮换）。
type RefreshVerifier func(ctx context.Context, token string, event string) (Account, error)

// InitRefreshProgress 创建进度对象。
func (s *AccountService) InitRefreshProgress(id string, total int) {
	if id == "" {
		id = uuid.NewString()
	}
	s.refreshProgress.Store(id, &Progress{
		ID:      id,
		Total:   total,
		Status:  "running",
		Started: time.Now(),
		StatusCounts: map[string]int{
			StatusNormal: 0, StatusLimited: 0, StatusAbnormal: 0, StatusDisabled: 0,
		},
	})
}

// UpdateRefreshProgress 记一个账号处理完成。
//
// 必须同时累加 processed、状态分布、额度合计 —— 前端进度条、统计卡片、
// 额度卡片分别读这三个字段（Python update_refresh_progress 同样三件事）。
func (s *AccountService) UpdateRefreshProgress(id, token string) {
	value, ok := s.refreshProgress.Load(id)
	if !ok {
		return
	}
	progress := value.(*Progress)

	// 账号信息在锁外读，避免 progress.mu 与 accounts.mu 交叉。
	account := s.GetAccount(token)
	status := StatusNormal
	quota := 0
	if account != nil {
		if raw := stringValue(account["status"]); strings.TrimSpace(raw) != "" {
			status = strings.TrimSpace(raw)
		}
		quota = maxInt(intValue(account["quota"]), 0)
	}

	progress.mu.Lock()
	progress.Processed++
	if progress.StatusCounts == nil {
		progress.StatusCounts = map[string]int{}
	}
	progress.StatusCounts[status]++
	progress.TotalQuota += quota
	progress.Current = anonymizeToken(token)
	progress.mu.Unlock()
}

func (s *AccountService) FinishRefreshProgress(id string, result map[string]any, errText string) {
	value, ok := s.refreshProgress.Load(id)
	if !ok {
		return
	}
	progress := value.(*Progress)
	progress.mu.Lock()
	progress.Done = true
	progress.Status = "done"
	progress.Error = errText
	progress.Result = result
	progress.Finished = time.Now()
	progress.mu.Unlock()
}

// GetRefreshProgress 读刷新进度。
//
// 已完成的进度在读取后**延迟一拍**清理：客户端靠轮询拿到 done=true，
// 若立刻删掉，紧接着的一次轮询会拿到 nil 而误判「进度不存在」。
// 这里在返回 done 的同时安排删除，让最后一次成功读取完整返回。
//
// 为什么需要清理：`refreshProgress` 是 sync.Map，每次
// POST /api/accounts/refresh 都存一个 *Progress，此前没有任何删除点
// （CleanRefreshProgress 零调用），管理员刷 200 次就留 200 个对象。
func (s *AccountService) GetRefreshProgress(id string) map[string]any {
	value, ok := s.refreshProgress.Load(id)
	if !ok {
		return nil
	}
	progress := value.(*Progress)
	snapshot := progress.snapshot("refresh")
	if done, _ := snapshot["done"].(bool); done && id != "" {
		// 同步删除即可：调用方已经拿到完整快照。
		s.refreshProgress.Delete(id)
	}
	return snapshot
}

func (s *AccountService) CleanRefreshProgress(id string) {
	s.refreshProgress.Delete(id)
}

func (s *AccountService) InitReloginProgress(id string, total int) {
	if id == "" {
		id = uuid.NewString()
	}
	s.reloginProgress.Store(id, &Progress{
		ID:      id,
		Total:   total,
		Status:  "running",
		Started: time.Now(),
	})
}

// UpdateReloginProgress 记一个账号重登结果。
//
// 前端读 `results[]` 渲染逐条状态（成功/禁用），并统计成功数与失败数，
// 所以每条都要 append 一个 {token,name,status,error} 条目 —— 与 Python
// update_relogin_progress 一致。只累加计数会让 UI 的反馈区永远空白。
func (s *AccountService) UpdateReloginProgress(id, token, status, errText string) {
	value, ok := s.reloginProgress.Load(id)
	if !ok {
		return
	}
	progress := value.(*Progress)

	if status == "running" {
		progress.mu.Lock()
		progress.Current = anonymizeToken(token)
		progress.mu.Unlock()
		return
	}

	// 账号名用于前端展示；取不到就退回脱敏 token。
	name := ""
	if account := s.GetAccount(token); account != nil {
		name = stringValue(account["email"])
	}
	if name == "" {
		name = anonymizeToken(token)
	}

	progress.mu.Lock()
	progress.Processed++
	progress.Results = append(progress.Results, map[string]any{
		"token":  anonymizeToken(token),
		"name":   name,
		"status": status,
		"error":  errText,
	})
	// 全部处理完即自动置 done（Python account_service.py:1458-1459）。
	// 缺这一步时，若调用方忘记调 FinishReloginProgress，前端会无限轮询。
	if progress.Total > 0 && progress.Processed >= progress.Total {
		progress.Done = true
	}
	if errText != "" {
		progress.Error = errText
	}
	progress.mu.Unlock()
}

func (s *AccountService) FinishReloginProgress(id string, result map[string]any, errText string) {
	value, ok := s.reloginProgress.Load(id)
	if !ok {
		return
	}
	progress := value.(*Progress)
	progress.mu.Lock()
	progress.Done = true
	progress.Status = "done"
	progress.Error = errText
	progress.Result = result
	progress.Finished = time.Now()
	progress.mu.Unlock()
}

// GetReloginProgress 读重登进度，已完成则清理（理由同 GetRefreshProgress）。
func (s *AccountService) GetReloginProgress(id string) map[string]any {
	value, ok := s.reloginProgress.Load(id)
	if !ok {
		return nil
	}
	progress := value.(*Progress)
	snapshot := progress.snapshot("relogin")
	if done, _ := snapshot["done"].(bool); done && id != "" {
		s.reloginProgress.Delete(id)
	}
	return snapshot
}

func (s *AccountService) CleanReloginProgress(id string) {
	s.reloginProgress.Delete(id)
}

// RefreshAccounts 并发刷新，最多 10 并发（与 Python 的 max_workers 一致）。
func (s *AccountService) RefreshAccounts(
	ctx context.Context,
	tokens []string,
	progressID string,
	verify RefreshVerifier,
) map[string]any {
	unique := make([]string, 0, len(tokens))
	seen := map[string]bool{}
	for _, token := range tokens {
		trimmed := strings.TrimSpace(token)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		// 跳过已被另一条路径在刷新的账号。
		//
		// 手工 POST /api/accounts/refresh 与 watcher 的 tick 之间没有互斥：
		// 两轮并发跑同一个 token 时，一次上游失效会被两条路径各记一次
		// invalid_count，账号更快被判定失效（实测 invalid_count=2）。
		// 用一个按账号的在飞标记去重，代价是那一轮跳过它 —— 下一轮还会刷。
		if !s.beginRefresh(trimmed) {
			continue
		}
		seen[trimmed] = true
		unique = append(unique, trimmed)
	}
	// 无论从哪条路径返回，都要释放已占住的刷新权。
	// 漏放会让账号永久卡在 refreshing 里 —— 之后再也刷不了它。
	defer func() {
		for _, token := range unique {
			s.endRefresh(token)
		}
	}()
	if len(unique) == 0 {
		result := map[string]any{
			"refreshed": 0,
			"errors":    []any{},
			"items":     s.ListAccounts(),
			"relogined": 0,
		}
		if progressID != "" {
			s.FinishRefreshProgress(progressID, result, "")
		}
		return result
	}
	if progressID != "" {
		s.InitRefreshProgress(progressID, len(unique))
	}

	type outcome struct {
		token   string
		account Account
		err     error
	}
	workers := 10
	if len(unique) < workers {
		workers = len(unique)
	}
	jobs := make(chan string)
	results := make(chan outcome, len(unique))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for token := range jobs {
				if verify == nil {
					results <- outcome{token: token}
					continue
				}
				account, err := verify(ctx, token, "refresh_accounts")
				results <- outcome{token: token, account: account, err: err}
			}
		}()
	}
	go func() {
		for _, token := range unique {
			jobs <- token
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	refreshed := 0
	errorsOut := make([]any, 0)
	// 本轮确认失效的 token，供 auto_relogin_after_refresh 自动重登用。
	invalidTokens := make([]string, 0)
	for item := range results {
		if item.err != nil {
			errorsOut = append(errorsOut, map[string]any{
				"token": anonymizeToken(item.token),
				"error": item.err.Error(),
			})
			// 记录失败原因，便于前端展示与排查（Python 版也会写 last_refresh_error）。
			s.recordRefreshError(item.token, item.err.Error())
			// token 真的失效（401/403）时走完整失效流程：invalid_count +1，
			// 再按 auto_remove_invalid_accounts 决定删号还是标记异常。
			// 限流（429）与网络错误不算失效 —— 否则一次上游抖动会删光号池。
			if isTokenInvalidFailure(item.err) {
				invalidTokens = append(invalidTokens, item.token)
				if s.RecordInvalidTokenSeen(item.token, "refresh", item.err.Error(), true) {
					s.RemoveInvalidToken(item.token, "refresh", false)
				}
			}
		} else if item.account != nil {
			// 关键：必须把拿到的账号信息写回号池。
			// 只计数不回写会让刷新「成功」但 email/quota 永远是空（实测踩到）。
			resolved := strings.TrimSpace(stringValue(item.account["access_token"]))
			if resolved == "" {
				resolved = item.token
			}
			if s.applyRefreshedAccount(item.token, resolved, item.account) {
				refreshed++
			}
		}
		if progressID != "" {
			s.UpdateRefreshProgress(progressID, item.token)
		}
	}

	// auto_relogin_after_refresh：本轮确认失效的账号，按配置自动尝试密码重登。
	//
	// 只在「刷新发现 401/403 且开关打开」时触发；429 限流与网络抖动不算失效
	// （与上面 isTokenInvalidFailure 的判定一致），否则一次上游抖动会批量重登。
	relogined := 0
	relogin := s.autoReloginFunc()
	if len(invalidTokens) > 0 && relogin != nil && s.cfg != nil && s.cfg.AutoReloginAfterRefresh() {
		reloginResult := s.ReloginAccounts(ctx, invalidTokens, "", relogin)
		relogined = intValue(reloginResult["relogined"])
		if errs, ok := reloginResult["errors"].([]any); ok {
			errorsOut = append(errorsOut, errs...)
		}
	}

	// 进度结果里**不放 items**。
	//
	// 原因：FinishRefreshProgress 会把整个 result 存进 Progress.Result，
	// 而 GET /api/accounts/refresh/progress/:id 原样返回 —— 于是刷新**一个**
	// 账号会泄漏**全池**的 password/totp_secret/refresh_token（实测确认）。
	//
	// 进度契约本来就只需要计数与错误：前端靠它显示「已处理 N/M」。
	// 需要账号列表的调用方应当再请求 GET /api/accounts（那里有脱敏）。
	result := map[string]any{
		"refreshed": refreshed,
		"errors":    errorsOut,
		"relogined": relogined,
	}
	if progressID != "" {
		s.FinishRefreshProgress(progressID, result, "")
	}
	return result
}

// SetAutoRelogin 注入自动重登用的密码登录实现。
//
// 由 api 层在启动时接线（service 不依赖 upstream）。为 nil 时
// auto_relogin_after_refresh 不生效 —— 与「开关打开但功能未接线」的区别是：
// 前者会打启动警告，后者不会。
func (s *AccountService) SetAutoRelogin(login PasswordLoginFunc) {
	s.mu.Lock()
	s.autoRelogin = login
	s.mu.Unlock()
}

// autoReloginFunc 在锁下取一次快照，避免与 SetAutoRelogin 形成数据竞争。
//
// 直接读 s.autoRelogin 是 race（写方持 s.mu，读方不持）。实践上它只在启动时
// 接线一次，所以影响很小 —— 但既然是竞态就该修，而且 SetAutoRelogin 自己
// 加锁说明作者本意是要安全的。
func (s *AccountService) autoReloginFunc() PasswordLoginFunc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.autoRelogin
}

// KeepaliveRefreshTokens 复刻 Python：批量 3 个、只对带 refresh_token 的账号。
//
// tokens 是 **access_token** 列表（来自 ListRefreshTokenKeepaliveTokens），
// 回调要的是这些账号的 **refresh_token** —— 两者不是一回事：
// refresh_token 才是换新 access_token 的凭据。早期实现把 access_token
// 直接传给回调，语义与 RefreshTokenFunc 的签名矛盾，且换到的新 token 不回写，
// keepalive 等于空转。两处都已修。
//
// 回调现在返回**完整三件套**：上游换 AT 时会轮换 refresh_token（实测 196 → 211 字符），
// 只取 access_token 会把新 RT 丢掉，账号此后一直用旧 RT 续期。
func (s *AccountService) KeepaliveRefreshTokens(
	ctx context.Context,
	tokens []string,
	refresh TokenRefreshFunc,
) map[string]any {
	errorsOut := make([]any, 0)
	refreshed := 0
	// RT 被上游拒绝的账号：交给 auto_relogin 用密码重登救回来。
	//
	// 为什么不能只记日志：RT 死了以后 AT 还能用到自然过期（最长 10 天），
	// 这期间账号看着「正常」但续期能力已经没了。等 AT 过期才发现就太晚 ——
	// 用户会看到账号莫名其妙全变成「异常」。这里立刻救，避免那个窗口。
	rejectedTokens := make([]string, 0)
	for start := 0; start < len(tokens); start += refreshTokenKeepaliveBatchSize {
		end := start + refreshTokenKeepaliveBatchSize
		if end > len(tokens) {
			end = len(tokens)
		}
		for _, token := range tokens[start:end] {
			if refresh == nil {
				continue
			}
			// 取该账号的 refresh_token；没有就跳过（Python 同样只处理带 rt 的号）。
			refreshToken := s.RefreshTokenFor(token)
			if refreshToken == "" {
				continue
			}
			rotated, err := refresh(ctx, refreshToken)
			if err != nil {
				// 本地 ctx 取消不是上游拒绝 —— 记进去会让整批账号被推进 6 小时退避。
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					s.RecordTokenRefreshError(token, err.Error())
				}
				errorsOut = append(errorsOut, map[string]any{
					"token": anonymizeToken(token),
					"error": err.Error(),
				})
				if IsRefreshTokenRejected(err) {
					rejectedTokens = append(rejectedTokens, token)
				}
				continue
			}
			// 回写完整三件套。
			//
			// access_token 为空时不能算成功：上游偶尔只回 refresh_token，
			// 此时 ApplyRotatedTokens 会写回 RT 但 AT 不变 —— 那不算一次有效刷新，
			// 计进 refreshed 会让面板显示「已续期」而实际 AT 还是旧的。
			if strings.TrimSpace(rotated["access_token"]) == "" {
				errorsOut = append(errorsOut, map[string]any{
					"token": anonymizeToken(token),
					"error": "上游未返回 access_token（仅回写 refresh_token）",
				})
				// 仍然调用：让它把轮换后的 RT 存下来，避免丢掉新凭据。
				s.ApplyRotatedTokens(token, rotated)
				continue
			}
			if !s.ApplyRotatedTokens(token, rotated) {
				errorsOut = append(errorsOut, map[string]any{
					"token": anonymizeToken(token),
					"error": "账号在刷新过程中消失，新 token 未回写",
				})
				continue
			}
			refreshed++
		}
	}
	// RT 被拒的账号立即尝试密码重登（与 RefreshAccounts 的失败路径同构）。
	relogined := 0
	relogin := s.autoReloginFunc()
	if len(rejectedTokens) > 0 && relogin != nil && s.cfg != nil && s.cfg.AutoReloginAfterRefresh() {
		reloginResult := s.ReloginAccounts(ctx, rejectedTokens, "", relogin)
		relogined = intValue(reloginResult["relogined"])
		if errs, ok := reloginResult["errors"].([]any); ok {
			errorsOut = append(errorsOut, errs...)
		}
	}

	return map[string]any{"refreshed": refreshed, "relogined": relogined, "errors": errorsOut}
}

// beginRefresh 尝试占住一个账号的刷新权，返回 false 表示已有另一轮在刷它。
func (s *AccountService) beginRefresh(token string) bool {
	s.refreshingMu.Lock()
	defer s.refreshingMu.Unlock()
	if s.refreshing == nil {
		s.refreshing = map[string]bool{}
	}
	if s.refreshing[token] {
		return false
	}
	s.refreshing[token] = true
	return true
}

// endRefresh 释放刷新权。
func (s *AccountService) endRefresh(token string) {
	s.refreshingMu.Lock()
	delete(s.refreshing, token)
	s.refreshingMu.Unlock()
}

// RefreshTokenFor 返回 access_token 对应账号的 refresh_token（不存在返回空）。
func (s *AccountService) RefreshTokenFor(accessToken string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	account, ok := s.accounts[s.resolveTokenLocked(accessToken)]
	if !ok {
		return ""
	}
	return strings.TrimSpace(stringValue(account["refresh_token"]))
}

// ApplyRotatedTokens 把 refresh_token 交换得到的整套 token 写回账号。
//
// 必须写三件套而不是只写 access_token：
//   - refresh_token：上游每次换 AT 都会轮换它（实测 196 → 211 字符）。
//     只写 AT 会把新 RT 丢掉，账号此后一直用旧 RT 续期，等于放弃上游给的新凭据。
//   - id_token：随 AT 一起轮换，导出/迁移时要用。
//
// 换 token 后旧 access_token 可能立刻失效，所以别名要跟着搬：否则调用方手里
// 还拿着旧 token 的引用会查不到账号。返回 false 表示账号已不存在。
//
// refresh_token 为空时不覆盖已有值 —— 上游偶尔不回传 RT，此时保留旧的是对的
// （旧 RT 仍可用，实测过）。
func (s *AccountService) ApplyRotatedTokens(oldAccessToken string, tokens map[string]string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldToken := s.resolveTokenLocked(oldAccessToken)
	account, ok := s.accounts[oldToken]
	if !ok {
		return false
	}
	newToken := strings.TrimSpace(tokens["access_token"])
	if newToken == "" {
		// 上游偶尔只回 refresh_token 不回 access_token（残缺响应）。
		// 此时**仍要写回轮换后的 RT** —— 上游在轮换时已把旧 RT 作废，
		// 丢掉新的会让账号手里只剩一个死 RT，续期能力彻底没了。
		// access_token 保持不变。
		if rt := strings.TrimSpace(tokens["refresh_token"]); rt != "" {
			updated := copyMap(account)
			updated["refresh_token"] = rt
			if id := strings.TrimSpace(tokens["id_token"]); id != "" {
				updated["id_token"] = id
			}
			updated["last_token_refresh_at"] = nowLocal()
			s.accounts[oldToken] = updated
			s.saveAccountsLocked()
		}
		return true
	}

	updated := copyMap(account)
	updated["access_token"] = newToken
	if rt := strings.TrimSpace(tokens["refresh_token"]); rt != "" {
		updated["refresh_token"] = rt
	}
	if id := strings.TrimSpace(tokens["id_token"]); id != "" {
		updated["id_token"] = id
	}
	now := nowLocal()
	updated["updated_at"] = now
	updated["updated_ts"] = float64(time.Now().UnixNano()) / 1e9
	// keepalive 锚点：写下这次成功刷新的时刻，下次从它 +3 天算起。
	updated["last_token_refresh_at"] = now
	// 刷新成功即清掉上一次的失败记录，让退避不再生效。
	updated["last_token_refresh_error"] = nil
	updated["last_token_refresh_error_at"] = nil

	if newToken == oldToken {
		s.accounts[oldToken] = updated
		s.saveAccountsLocked()
		return true
	}
	delete(s.accounts, oldToken)
	s.accounts[newToken] = updated
	// 旧 token 指向新 token，让仍持有旧引用的调用方还能找到账号。
	s.aliases[oldToken] = newToken

	// 清掉旧 token 的槽位与在飞计数。
	//
	// 不清的后果：每次 keepalive 轮换都留下一个孤儿 channel（容量
	// image_account_concurrency，默认 3）和一个 *atomic.Int64。
	// ReleaseImageSlot 会把别名解析到**新** token，所以旧 channel 永远不会
	// 被排空 —— 按 3 天 keepalive 的号池寿命，这会无界增长。
	// 两个兄弟函数（applyRefreshedAccount / ApplyReloginTokens）都做了这一步，
	// 只有这里漏了。
	s.slotsMu.Lock()
	delete(s.slots, oldToken)
	s.slotsMu.Unlock()
	s.inflight.Delete(oldToken)

	s.saveAccountsLocked()
	return true
}

// RecordTokenRefreshError 记下 keepalive 失败的时刻与原因。
//
// 为什么必须有它：recentRefreshTokenKeepaliveErrorLocked 读
// last_token_refresh_error_at 来决定是否退避 refreshKeepaliveErrorBackoff（6 小时）。
// 此前没有任何代码写这个字段，退避恒定失效 —— RT 失效的账号会被每轮 tick
// （5 分钟）重试一次，而不是退避 6 小时。
func (s *AccountService) RecordTokenRefreshError(accessToken, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	account, ok := s.accounts[s.resolveTokenLocked(accessToken)]
	if !ok {
		return
	}
	updated := copyMap(account)
	updated["last_token_refresh_error"] = reason
	updated["last_token_refresh_error_at"] = nowLocal()
	s.accounts[s.resolveTokenLocked(accessToken)] = updated
	s.saveAccountsLocked()
}

// BuildExportItems 复刻 Python：只导出三件套齐全的账号。
func (s *AccountService) BuildExportItems(tokens []string) []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	targets := map[string]bool{}
	for _, token := range tokens {
		if trimmed := strings.TrimSpace(token); trimmed != "" {
			targets[s.resolveTokenLocked(trimmed)] = true
		}
	}
	out := make([]map[string]any, 0)
	for token, account := range s.accounts {
		if len(targets) > 0 && !targets[token] {
			continue
		}
		refreshToken := stringValue(account["refresh_token"])
		idToken := stringValue(account["id_token"])
		if token == "" || refreshToken == "" || idToken == "" {
			continue
		}
		item := map[string]any{
			"access_token":  token,
			"refresh_token": refreshToken,
			"id_token":      idToken,
		}
		if exportType := stringValue(account["export_type"]); exportType != "" {
			item["export_type"] = exportType
		}
		// password / totp_secret 必须一起导出：否则 export → import 之后
		// 每个账号都变成 no_password_in_account，密码重登功能对导出的号池失效。
		// Python 的导出同样带 password（services/account_service.py）。
		if password := stringValue(account["password"]); password != "" {
			item["password"] = password
		}
		if totpSecret := stringValue(account["totp_secret"]); totpSecret != "" {
			item["totp_secret"] = totpSecret
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return stringValue(out[i]["access_token"]) < stringValue(out[j]["access_token"])
	})
	return out
}

// ErrProgressNotFound 对应 Python 的 404 "progress not found"。
var ErrProgressNotFound = errors.New("progress not found")

// applyRefreshedAccount 把刷新拿到的账号信息合并进号池。
//
// 合并规则（与 Python 的 fetch_remote_info 一致）：
//   - 只覆盖「上游能给出的」字段，保留本地维护的字段（proxy、created_at、success/fail 等）
//   - 若 access_token 发生轮换，把旧 token 记进别名表，让前端手里的旧 token 仍能解析
//
// 返回 true 表示确实写入了。
func (s *AccountService) applyRefreshedAccount(oldToken, resolvedToken string, incoming Account) bool {
	if incoming == nil {
		return false
	}
	resolved := strings.TrimSpace(resolvedToken)
	if resolved == "" {
		resolved = strings.TrimSpace(oldToken)
	}
	if resolved == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.accounts[resolved]
	if current == nil {
		// 刷新期间 token 轮换过：先按旧 token 找。
		if previous := s.accounts[oldToken]; previous != nil {
			current = previous
		} else {
			// 号池里没有（可能已被删除），不凭空创建。
			return false
		}
	}

	merged := copyMap(current)
	for _, key := range []string{
		"email", "user_id", "type", "quota", "limits_progress",
		"default_model_slug", "restore_at", "status",
		"last_refresh_error", "last_refresh_error_at",
	} {
		if value, ok := incoming[key]; ok {
			merged[key] = value
		}
	}
	// 上游刷新成功即清掉上一次的刷新错误。
	merged["last_refresh_error"] = nil
	merged["last_refresh_error_at"] = nil
	merged["access_token"] = resolved
	if refreshedAt := stringValue(incoming["last_token_refresh_at"]); refreshedAt != "" {
		merged["last_token_refresh_at"] = refreshedAt
	}

	account := normalizeAccount(merged)
	if account == nil {
		return false
	}

	// token 轮换：旧 token 指向新 token，并删掉旧条目。
	if oldToken != "" && oldToken != resolved {
		delete(s.accounts, oldToken)
		s.aliases[oldToken] = resolved
		s.slotsMu.Lock()
		delete(s.slots, oldToken)
		s.slotsMu.Unlock()
		s.inflight.Delete(oldToken)
	}

	s.accounts[resolved] = account
	s.saveAccountsLocked()
	return true
}

// recordRefreshError 记录一次刷新失败（不改变账号状态，只留痕）。
func (s *AccountService) recordRefreshError(token, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	resolved := s.resolveTokenLocked(token)
	current := s.accounts[resolved]
	if current == nil {
		return
	}
	next := copyMap(current)
	next["last_refresh_error"] = strings.TrimSpace(message)
	// 同类：用 nowLocal 与 last_invalid_at（同为本地）保持一致。
	next["last_refresh_error_at"] = nowLocal()
	if account := normalizeAccount(next); account != nil {
		s.accounts[resolved] = account
		s.saveAccountsLocked()
	}
}
