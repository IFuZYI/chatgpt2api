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
			current = map[string]any{"created_at": nowString()}
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

func (s *AccountService) GetRefreshProgress(id string) map[string]any {
	value, ok := s.refreshProgress.Load(id)
	if !ok {
		return nil
	}
	return value.(*Progress).snapshot("refresh")
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

func (s *AccountService) GetReloginProgress(id string) map[string]any {
	value, ok := s.reloginProgress.Load(id)
	if !ok {
		return nil
	}
	return value.(*Progress).snapshot("relogin")
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
		seen[trimmed] = true
		unique = append(unique, trimmed)
	}
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

	result := map[string]any{
		"refreshed": refreshed,
		"errors":    errorsOut,
		"items":     s.ListAccounts(),
		"relogined": 0,
	}
	if progressID != "" {
		s.FinishRefreshProgress(progressID, result, "")
	}
	return result
}

// KeepaliveRefreshTokens 复刻 Python：批量 3 个、只对带 refresh_token 的账号。
func (s *AccountService) KeepaliveRefreshTokens(
	ctx context.Context,
	tokens []string,
	refresh func(ctx context.Context, token string) (string, error),
) map[string]any {
	errorsOut := make([]any, 0)
	refreshed := 0
	for start := 0; start < len(tokens); start += refreshTokenKeepaliveBatchSize {
		end := start + refreshTokenKeepaliveBatchSize
		if end > len(tokens) {
			end = len(tokens)
		}
		for _, token := range tokens[start:end] {
			if refresh == nil {
				continue
			}
			if _, err := refresh(ctx, token); err != nil {
				errorsOut = append(errorsOut, map[string]any{
					"token": anonymizeToken(token),
					"error": err.Error(),
				})
				continue
			}
			refreshed++
		}
	}
	return map[string]any{"refreshed": refreshed, "errors": errorsOut}
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
	next["last_refresh_error_at"] = nowString()
	if account := normalizeAccount(next); account != nil {
		s.accounts[resolved] = account
		s.saveAccountsLocked()
	}
}
