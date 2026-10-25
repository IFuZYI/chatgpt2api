package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/config"
	"github.com/IFuZYI/chatgpt2api/internal/storage"
	"github.com/google/uuid"
)

// 账号状态。这四个值是磁盘上的字面量，也是前端筛选依据，不能改。
const (
	StatusNormal   = "正常"
	StatusLimited  = "限流"
	StatusAbnormal = "异常"
	StatusDisabled = "禁用"
)

// source_type 取值。
const (
	SourceWeb   = "web"
	SourceCodex = "codex"
	// SourcePassword 表示账号是通过邮箱密码登录（含 TOTP）拿到 token 的。
	SourcePassword = "password"
)

// Account 是号池里的一个账号。
//
// 字段集就是 data/accounts.json 的 schema —— 对外 API 与前端都直接读它，
// 所以这里用 map[string]any 作为权威表示，AccessToken 只做索引键。
// 强类型结构体会在回写时把缺失字段写成零值，污染用户数据（见 ADR-0006）。
type Account = map[string]any

// AccountService 复刻 services/account_service.py 的核心状态机。
//
// 并发模型（ADR-0007）：
//   - mu 保护 accounts 映射、别名表、游标
//   - 槽位是每账号一个带缓冲 channel，容量 = image_account_concurrency
//   - 获取槽位时若「有就绪账号但都满」则阻塞等待；「无就绪账号」立即报错
type AccountService struct {
	mu      sync.RWMutex
	storage storage.Backend
	cfg     *config.Store
	logs    *LogService

	accounts map[string]Account
	aliases  map[string]string
	cursor   atomic.Uint64
	// autoRelogin 是 auto_relogin_after_refresh 用的密码登录实现。
	// 由 api 层注入（service 不依赖 upstream）；nil 表示未接线。
	autoRelogin PasswordLoginFunc
	// version 在号池内容变化时自增。
	//
	// 存在的理由：ModelCatalog 需要判断「号池是否变了」来决定缓存是否失效。
	// 原来的做法是每次请求都遍历全部账号构造签名 —— 269 个账号时占
	// /v1/models 总耗时的 64%（实测 101μs / 159μs）。有了版本号，
	// 这个判断变成一次原子读。
	version atomic.Uint64
	// structuralSig 是上次 bumpVersionLocked 时的结构性签名。
	// 只有它变化才自增 version（使用统计变化不算）。
	structuralSig string

	// slots 每账号一个信号量；惰性创建，删除账号时清理。
	slotsMu sync.Mutex
	slots   map[string]chan struct{}
	// inflight 只用于展示与诊断（/api/accounts 的 image_inflight 字段）。
	inflight sync.Map // token -> *atomic.Int64

	refreshProgress sync.Map // progressID -> *Progress
	reloginProgress sync.Map // progressID -> *Progress

	// refreshing 标记「正在被某条路径刷新」的账号，防止手工刷新与
	// watcher tick 并发处理同一个 token（会让一次上游失效被计两次）。
	refreshingMu sync.Mutex
	refreshing   map[string]bool

	cumulativeTotal int
	cumulativeMu    sync.Mutex

	// slotWaiters 用于「有就绪账号但都满」时的唤醒。
	slotCond *sync.Cond
}

const (
	newAccountInvalidGraceSeconds  = 10 * 60
	invalidConfirmSeconds          = 30
	accessTokenRefreshSkewSeconds  = 24 * 60 * 60
	refreshTokenKeepaliveSeconds   = 3 * 24 * 60 * 60
	refreshKeepaliveErrorBackoff   = 6 * 60 * 60
	refreshTokenKeepaliveBatchSize = 3
	tokenRefreshErrorBackoff       = 5 * 60
)

const (
	oauthTokenURL = "https://auth.openai.com/oauth/token"
	// 这两个常量在 oauth_login.go 里也用；导出别名见下方 const 块。
	oauthClientID  = "app_2SKx67EdpoN0G6j64rFvigXD"
	oauthUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
)

// Progress 是刷新/重登的进度对象。
//
// 字段与 Python account_service 的进度字典逐字对齐 —— 这不是内部结构，
// 它是前端轮询接口的响应契约：
//
//	刷新：{total, processed, done:bool, error, status_counts, total_quota, result}
//	重登：{total, processed, done:bool, error, results[]}
//
// 特别注意 `done` 是 **bool**（是否结束），已处理条数是 `processed`。
// 早期实现把两者合并成一个 `Done int` 并序列化到 `done` 字段，
// 前端 `if (p.done)` 于是在第一个账号刷完时就判定「已完成」，
// 停止轮询并报「刷新结果为空」—— 账号页的刷新/恢复功能因此完全不可用。
type Progress struct {
	mu        sync.Mutex
	ID        string
	Total     int
	Processed int
	Done      bool
	Current   string
	Status    string
	Error     string
	// StatusCounts 仅刷新进度使用：{"正常":n,"限流":n,"异常":n,"禁用":n}
	StatusCounts map[string]int
	// TotalQuota 仅刷新进度使用：已处理账号的额度合计
	TotalQuota int
	// Results 仅重登进度使用：每条 {token,name,status,error}
	Results  []map[string]any
	Result   map[string]any
	Started  time.Time
	Finished time.Time
}

// snapshot 输出前端轮询契约。
//
// kind 决定附加字段：refresh 带 status_counts/total_quota，relogin 带 results。
// 两个接口共用同一个 Progress 类型，但字段集不同 —— 与 Python 的两个
// 独立字典一致，前端也按各自的类型声明读取。
func (p *Progress) snapshot(kind string) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := map[string]any{
		"total":     p.Total,
		"processed": p.Processed,
		"done":      p.Done,
	}
	if p.Error != "" {
		out["error"] = p.Error
	}

	switch kind {
	case "refresh":
		counts := make(map[string]any, len(p.StatusCounts))
		for key, value := range p.StatusCounts {
			counts[key] = value
		}
		// 四个状态键必须始终存在（前端直接索引，缺键会读到 undefined）。
		for _, key := range []string{StatusNormal, StatusLimited, StatusAbnormal, StatusDisabled} {
			if _, ok := counts[key]; !ok {
				counts[key] = 0
			}
		}
		out["status_counts"] = counts
		out["total_quota"] = p.TotalQuota
		if p.Result != nil {
			out["result"] = p.Result
		}
	case "relogin":
		results := make([]any, 0, len(p.Results))
		for _, item := range p.Results {
			results = append(results, copyMap(item))
		}
		out["results"] = results
	}

	return out
}

func NewAccountService(backend storage.Backend, cfg *config.Store, logs *LogService) *AccountService {
	s := &AccountService{
		storage:  backend,
		cfg:      cfg,
		logs:     logs,
		accounts: map[string]Account{},
		aliases:  map[string]string{},
		slots:    map[string]chan struct{}{},
	}
	s.slotCond = sync.NewCond(&s.mu)
	s.accounts = s.loadAccounts()
	s.cumulativeTotal = s.loadCumulativeTotal()
	// 建立结构性签名基线：否则首次 saveAccountsLocked 会把「空串 → 真实签名」
	// 当成一次结构性变化，白白自增一次版本号。
	s.structuralSig = s.structuralSignatureLocked()
	return s
}

func (s *AccountService) loadAccounts() map[string]Account {
	raw, err := s.storage.LoadAccounts()
	if err != nil {
		return map[string]Account{}
	}
	out := make(map[string]Account, len(raw))
	for _, item := range raw {
		normalized := normalizeAccount(item)
		if normalized == nil {
			continue
		}
		token, _ := normalized["access_token"].(string)
		out[token] = normalized
	}
	return out
}

// saveAccountsLocked 把号池落盘。
//
// 写失败必须留下痕迹：早期实现 `_ = s.storage.SaveAccounts(items)` 全吞，
// 磁盘满 / 权限错 / 数据库断连时接口仍返 200，用户以为保存成功，
// 重启后数据全部丢失（Python 会抛异常冒泡到 500）。
// 这里保留接口的成功语义（避免一次写失败把整轮请求打挂），
// 但把错误写进日志，运维能查到。
func (s *AccountService) saveAccountsLocked() {
	items := make([]map[string]any, 0, len(s.accounts))
	for _, account := range s.accounts {
		items = append(items, account)
	}
	// 先自增再落盘：即使落盘失败，内存里的号池也变了，
	// 缓存必须失效（否则会返回基于旧号池的模型列表）。
	//
	// 注意：这里用 bumpVersion 而不是无条件自增 —— 见它的注释。
	s.bumpVersionLocked()
	if err := s.storage.SaveAccounts(items); err != nil {
		s.logSaveError("accounts", err)
	}
}

// logSaveError 记录一次落盘失败。
func (s *AccountService) logSaveError(kind string, err error) {
	if s.logs != nil {
		s.logs.Add(LogTypeAccount, "号池落盘失败", map[string]any{
			"kind":  kind,
			"error": err.Error(),
		})
	}
	fmt.Fprintf(os.Stderr, "[account] save %s failed: %v\n", kind, err)
}

// saveAccounts 是带锁版本，供未持锁的调用点使用。
func (s *AccountService) saveAccounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveAccountsLocked()
}

func (s *AccountService) cumulativeFile() string {
	return s.cfg.DataDir() + "/.cumulative_total"
}

func (s *AccountService) loadCumulativeTotal() int {
	payload, err := readFileString(s.cumulativeFile())
	if err == nil {
		var parsed int
		if _, scanErr := fmt.Sscanf(strings.TrimSpace(payload), "%d", &parsed); scanErr == nil {
			return parsed
		}
	}
	return len(s.accounts)
}

func (s *AccountService) saveCumulativeTotal() {
	_ = writeFileString(s.cumulativeFile(), fmt.Sprintf("%d", s.cumulativeTotal))
}

func (s *AccountService) bumpCumulative(delta int) {
	s.cumulativeMu.Lock()
	s.cumulativeTotal += delta
	total := s.cumulativeTotal
	s.cumulativeMu.Unlock()
	_ = total
	s.saveCumulativeTotal()
}

// normalizeAccount 复刻 Python 的 _normalize_account。
// 返回 nil 表示这条记录无法修复（缺 access_token）。
func normalizeAccount(item map[string]any) Account {
	if item == nil {
		return nil
	}
	// 必须 TrimSpace：导出的 token 文件常带行尾换行（\n / \r\n），
	// 原样拼进 Authorization 头会被 Go 判为非法头值，请求直接发不出去
	// （症状是 "invalid HTTP header value ... for header authorization"）。
	// Python 版这里是 str(...).strip()，Go 版必须对齐。
	accessToken := strings.TrimSpace(stringValue(item["access_token"]))
	if accessToken == "" {
		accessToken = strings.TrimSpace(stringValue(item["accessToken"]))
	}
	if accessToken == "" {
		return nil
	}
	out := copyMap(item)
	delete(out, "accessToken")
	out["access_token"] = accessToken

	if strings.ToLower(strings.TrimSpace(stringValue(out["type"]))) == "codex" {
		out["export_type"] = "codex"
		delete(out, "type")
	}
	if stringValue(out["type"]) == "" {
		out["type"] = "free"
	}
	if stringValue(out["status"]) == "" {
		out["status"] = StatusNormal
	}
	out["quota"] = maxInt(intValue(out["quota"]), 0)
	out["email"] = nilIfEmpty(stringValue(out["email"]))
	out["user_id"] = nilIfEmpty(stringValue(out["user_id"]))
	out["proxy"] = strings.TrimSpace(stringValue(out["proxy"]))

	sourceType := stringValue(out["source_type"])
	if sourceType == "" && strings.ToLower(strings.TrimSpace(stringValue(out["export_type"]))) == "codex" {
		sourceType = SourceCodex
	}
	out["source_type"] = normalizeSourceType(sourceType)

	if _, ok := out["limits_progress"].([]any); !ok {
		out["limits_progress"] = []any{}
	}
	out["default_model_slug"] = nilIfEmpty(stringValue(out["default_model_slug"]))
	out["restore_at"] = nilIfEmpty(stringValue(out["restore_at"]))
	out["success"] = intValue(out["success"])
	out["fail"] = intValue(out["fail"])
	out["invalid_count"] = intValue(out["invalid_count"])
	out["last_invalid_at"] = nilIfEmpty(stringValue(out["last_invalid_at"]))
	out["last_refresh_error"] = nilIfEmpty(stringValue(out["last_refresh_error"]))
	out["last_refresh_error_at"] = nilIfEmpty(stringValue(out["last_refresh_error_at"]))
	out["last_token_refresh_at"] = nilIfEmpty(stringValue(out["last_token_refresh_at"]))
	out["last_token_refresh_error"] = nilIfEmpty(stringValue(out["last_token_refresh_error"]))
	out["last_token_refresh_error_at"] = nilIfEmpty(stringValue(out["last_token_refresh_error_at"]))
	if stringValue(out["created_at"]) == "" {
		// 用 nowLocal 而不是 nowString：账号的所有时间字段
		// （created_at / last_invalid_at / last_token_refresh_at /
		// last_token_refresh_error_at）都必须用同一种时区写，
		// 否则 parseAccountTime 无法用一个规则解析它们。
		//
		// 原来这里用 nowString()（UTC），其余用 nowLocal()（本地）。
		// 在 UTC+X 部署下 created_at 比真实时间早 X 小时 ——
		// shouldDeferInvalidTokenLocked 的「新号宽限」对刚导入的号永不生效；
		// 在 UTC-X 部署下 created_at 落在未来 —— 宽限恒真，账号永不可删。
		// 两个方向都错，且只有非 UTC 部署才暴露。
		//
		// 历史数据里已有的 created_at 是 UTC：偏差只影响「新号宽限」
		// 这一个判断（窗口 10 分钟），而历史账号都远超 10 分钟，
		// 所以不需要迁移。
		out["created_at"] = nowLocal()
	}
	return out
}

func normalizeSourceType(value any) string {
	text := strings.ToLower(strings.TrimSpace(stringValue(value)))
	if text == "" {
		return SourceWeb
	}
	return text
}

// normalizeAccountType 复刻别名表：business→Team、prolite→ProLite 等。
func normalizeAccountType(value any) string {
	raw := strings.TrimSpace(stringValue(value))
	if raw == "" {
		return ""
	}
	key := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(raw, "-", "_"), " ", "_"))
	compact := strings.ReplaceAll(key, "_", "")
	aliases := map[string]string{
		"free":       "free",
		"plus":       "Plus",
		"pro":        "Pro",
		"prolite":    "ProLite",
		"team":       "Team",
		"business":   "Team",
		"enterprise": "Enterprise",
	}
	if mapped, ok := aliases[compact]; ok {
		return mapped
	}
	if mapped, ok := aliases[key]; ok {
		return mapped
	}
	return raw
}

func (s *AccountService) isImageAccountAvailable(account Account) bool {
	if account == nil {
		return false
	}
	switch stringValue(account["status"]) {
	case StatusDisabled, StatusLimited, StatusAbnormal:
		return false
	}
	return intValue(account["quota"]) > 0
}

func accountMatchesPlanType(account Account, planType string) bool {
	if planType == "" {
		return true
	}
	normalizedPlan := normalizeAccountType(planType)
	normalizedAccount := normalizeAccountType(account["type"])
	if normalizedPlan == "" || normalizedAccount == "" {
		return false
	}
	return strings.EqualFold(normalizedPlan, normalizedAccount)
}

func accountMatchesSourceType(account Account, sourceType string) bool {
	if sourceType == "" {
		return true
	}
	return normalizeSourceType(account["source_type"]) == normalizeSourceType(sourceType)
}

func accountMatchesAnyPlanType(account Account, planTypes []string) bool {
	if len(planTypes) == 0 {
		return true
	}
	normalizedAccount := normalizeAccountType(account["type"])
	if normalizedAccount == "" {
		return false
	}
	for _, planType := range planTypes {
		if normalizeAccountType(planType) == normalizedAccount {
			return true
		}
	}
	return false
}

// resolveTokenLocked 顺着别名链把旧 token 解析到当前 token。
func (s *AccountService) resolveTokenLocked(token string) string {
	current := strings.TrimSpace(token)
	seen := map[string]bool{}
	for current != "" && s.accounts[current] == nil && s.aliases[current] != "" && !seen[current] {
		seen[current] = true
		current = s.aliases[current]
	}
	return current
}

// ResolveAccessToken 是带锁的对外版本。
func (s *AccountService) ResolveAccessToken(token string) string {
	if token == "" {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resolveTokenLocked(token)
}

// ListAccounts 返回账号副本，按磁盘顺序（map 迭代顺序不稳定，这里按 created_at+token 排序保证稳定）。
func (s *AccountService) ListAccounts() []Account {
	s.mu.RLock()
	items := make([]Account, 0, len(s.accounts))
	for _, account := range s.accounts {
		items = append(items, copyMap(account))
	}
	s.mu.RUnlock()

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

func (s *AccountService) ListTokens() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.accounts))
	for token := range s.accounts {
		out = append(out, token)
	}
	sort.Strings(out)
	return out
}

// GetAccount 返回副本。
func (s *AccountService) GetAccount(token string) Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	resolved := s.resolveTokenLocked(token)
	account := s.accounts[resolved]
	if account == nil {
		return nil
	}
	return copyMap(account)
}

// Stats 复刻 Python 的 get_stats()，字段名是 /health 的契约。
func (s *AccountService) Stats() map[string]any {
	s.mu.RLock()
	items := make([]Account, 0, len(s.accounts))
	for _, account := range s.accounts {
		items = append(items, account)
	}
	s.mu.RUnlock()

	total := len(items)
	var active, limited, abnormal, disabled, totalQuota, totalSuccess, totalFail int
	byType := map[string]int{}
	for _, account := range items {
		switch stringValue(account["status"]) {
		case StatusNormal:
			active++
			totalQuota += maxInt(intValue(account["quota"]), 0)
		case StatusLimited:
			limited++
		case StatusAbnormal:
			abnormal++
		case StatusDisabled:
			disabled++
		}
		totalSuccess += intValue(account["success"])
		totalFail += intValue(account["fail"])
		typeName := stringValue(account["type"])
		if typeName == "" {
			typeName = "unknown"
		}
		byType[typeName]++
	}

	s.cumulativeMu.Lock()
	cumulative := s.cumulativeTotal
	s.cumulativeMu.Unlock()

	// by_type 需要转成 map[string]any 以便 JSON 序列化后与 Python 一致。
	byTypeAny := make(map[string]any, len(byType))
	for key, value := range byType {
		byTypeAny[key] = value
	}

	return map[string]any{
		"total":            total,
		"cumulative_total": cumulative,
		"active":           active,
		"limited":          limited,
		"abnormal":         abnormal,
		"disabled":         disabled,
		"total_quota":      totalQuota,
		"total_success":    totalSuccess,
		"total_fail":       totalFail,
		"by_type":          byTypeAny,
	}
}

// AccountHealth 复刻 Python 的 account_health()。
func (s *AccountService) AccountHealth() map[string]any {
	stats := s.Stats()
	active := intValue(stats["active"])
	out := map[string]any{
		"healthy": active > 0,
		"status":  map[bool]string{true: "ok", false: "degraded"}[active > 0],
	}
	for key, value := range stats {
		out[key] = value
	}
	return out
}

// ListLimitedTokens / ListNormalTokens 供后台 watcher 使用。
func (s *AccountService) ListLimitedTokens() []string {
	return s.tokensByStatus(StatusLimited)
}

func (s *AccountService) ListNormalTokens() []string {
	return s.tokensByStatus(StatusNormal)
}

func (s *AccountService) tokensByStatus(status string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.accounts))
	for token, account := range s.accounts {
		if stringValue(account["status"]) == status {
			out = append(out, token)
		}
	}
	sort.Strings(out)
	return out
}

// ListExpiringAccessTokens 返回 24h 内过期、且带 refresh_token 的账号。
func (s *AccountService) ListExpiringAccessTokens() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0)
	for token, account := range s.accounts {
		if stringValue(account["refresh_token"]) == "" {
			continue
		}
		remaining := tokenExpiresIn(token)
		if remaining != nil && *remaining <= accessTokenRefreshSkewSeconds {
			out = append(out, token)
		}
	}
	sort.Strings(out)
	return out
}

// ListAbnormalTokens 返回被标记为「异常」的账号。
//
// 为什么需要它：`异常` 原本是**终态** —— watcher 只挑 limited/normal/expiring，
// 而 expiring 与 keepalive 都要求账号有 refresh_token。对没有 RT 的账号
// （本仓库实测 260 个号里 256 个属于此类），一旦被 401 打成 `异常`，
// 就再没有任何列表会选中它，只能人工处理。
//
// 纳入重试后：上游恢复时 applyRefreshedAccount 会把 status 合并回「正常」，
// 账号自动救回；上游仍失效则继续 401（有 shouldDeferInvalidTokenLocked 兜底，
// 不会误删）。
//
// 退避策略：invalid_count 越大，重试间隔越长（指数退避，封顶 6 小时）。
//
// 为什么不能简单地「超过 N 次就不再重试」：那样一个真正失效的号确实不再
// 浪费请求，但**上游恢复后它也永远回不来** —— 账号卡在异常状态，只能人工处理。
// 而这批账号大多没有 refresh_token（本仓库实测 260 个里 256 个），
// 人工之外没有别的恢复路径。
//
// 改成按次数退避：刚失效时每轮都试（快速捕获上游抖动恢复），
// 之后间隔按 2^n 拉长到 6 小时封顶 —— 一个死号一天最多 4 次请求，
// 代价可以忽略，但恢复能力保住了。
//
// 锚点用 last_invalid_at（每次 401 都会刷新），所以退避窗口是相对
// 「最后一次失败」而不是「第一次失败」。
const (
	abnormalRetryBaseDelay = 5 * time.Minute
	abnormalRetryMaxDelay  = 6 * time.Hour
)

func (s *AccountService) ListAbnormalTokens() []string {
	now := time.Now().UTC()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0)
	for token, account := range s.accounts {
		if stringValue(account["status"]) != StatusAbnormal {
			continue
		}
		if !s.abnormalRetryDueLocked(account, now) {
			continue
		}
		out = append(out, token)
	}
	sort.Strings(out)
	return out
}

// abnormalRetryDueLocked 判断异常账号是否到了下一次重试时间。
func (s *AccountService) abnormalRetryDueLocked(account Account, now time.Time) bool {
	count := intValue(account["invalid_count"])
	if count <= 1 {
		return true // 刚失效，立即重试（捕获上游抖动恢复）
	}
	delay := abnormalRetryBaseDelay
	for i := 1; i < count && delay < abnormalRetryMaxDelay; i++ {
		delay *= 2
	}
	if delay > abnormalRetryMaxDelay {
		delay = abnormalRetryMaxDelay
	}
	last := parseAccountTime(account["last_invalid_at"])
	if last == nil {
		return true
	}
	if last.After(now) {
		return true // 未来时间戳（时钟错乱）：当作已到期，避免永久冻结
	}
	return now.Sub(*last) >= delay
}

// ListRefreshTokenKeepaliveTokens 返回 refresh_token 接近闲置上限的账号。
func (s *AccountService) ListRefreshTokenKeepaliveTokens() []string {
	now := time.Now().UTC()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0)
	for token, account := range s.accounts {
		if stringValue(account["refresh_token"]) == "" {
			continue
		}
		if s.recentRefreshTokenKeepaliveErrorLocked(account, now) {
			continue
		}
		dueAt := s.refreshTokenKeepaliveDueAtLocked(account, now)
		if dueAt != nil && !dueAt.After(now) {
			out = append(out, token)
		}
	}
	sort.Strings(out)
	return out
}

// ---- JWT 工具 ----

func decodeJWTPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	payload := parts[1]
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(decoded, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func jwtExp(token string) int64 {
	return int64(intValue(decodeJWTPayload(token)["exp"]))
}

func tokenExpiresIn(token string) *int {
	exp := jwtExp(token)
	if exp <= 0 {
		return nil
	}
	remaining := int(exp - time.Now().Unix())
	return &remaining
}

func tokenNeedsRefresh(token string, force bool) bool {
	if force {
		return true
	}
	remaining := tokenExpiresIn(token)
	return remaining != nil && *remaining <= accessTokenRefreshSkewSeconds
}

func tokenIssuedAt(token string) *time.Time {
	iat := int64(intValue(decodeJWTPayload(token)["iat"]))
	if iat <= 0 {
		return nil
	}
	issued := time.Unix(iat, 0).UTC()
	return &issued
}

// ---- 内部时间解析 ----

// parseAccountTime 兼容 "2006-01-02 15:04:05" 与 RFC3339（含 Z）。
func parseAccountTime(value any) *time.Time {
	raw := strings.TrimSpace(stringValue(value))
	if raw == "" {
		return nil
	}
	// 带时区的格式：按字面解析，结果已明确。
	zonedLayouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05Z07:00", // 空格分隔 + 数字偏移（第三方导入可能产生）
		"2006-01-02 15:04:05-07:00",
	}
	normalized := strings.Replace(raw, "Z", "+00:00", 1)
	for _, layout := range zonedLayouts {
		if parsed, err := time.Parse(layout, normalized); err == nil {
			utc := parsed.UTC()
			return &utc
		}
	}

	// 不带时区的格式：必须按**本地时区**解析，因为写入方用的是
	// nowLocal() / now.Format(...) —— 都是本地时间且不带偏移。
	//
	// 原来这里用 time.Parse（按 UTC），于是 UTC+X 部署下解析结果比真实时间
	// 早 X 小时：keepalive 锚点提前 X 小时到期、6 小时错误退避永远不生效
	// （now.Sub(未来) 为负，负数恒小于 6h）。实测 UTC+8 偏差 -8h、
	// UTC-7 偏差 +7h。
	//
	// 用 ParseInLocation(time.Local) 与写入方对齐。历史数据同样是本地时间，
	// 所以这个改动对已有记录也是正确的（不需要迁移）。
	naiveLayouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04:05.999999999",
	}
	for _, layout := range naiveLayouts {
		if parsed, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
}

func nowString() string {
	return time.Now().UTC().Format("2006-01-02 15:04:05")
}

func (s *AccountService) refreshTokenKeepaliveAnchorLocked(account Account) *time.Time {
	var anchor *time.Time
	if parsed := parseAccountTime(account["last_token_refresh_at"]); parsed != nil {
		anchor = parsed
	} else if issued := tokenIssuedAt(stringValue(account["access_token"])); issued != nil {
		anchor = issued
	} else {
		anchor = parseAccountTime(account["created_at"])
	}
	// 未来时间戳钳制到现在。
	//
	// 未来锚点会让「锚点 +3 天」永远在更远的未来，账号永久不被 keepalive 选中。
	// 产生未来值的原因：系统时钟错乱、从备份恢复、或历史版本在 UTC+X 部署下
	// 用本地时间写入了 UTC 字段。钳制是廉价保险。
	if anchor != nil {
		if now := time.Now().UTC(); anchor.After(now) {
			anchor = &now
		}
	}
	return anchor
}

func (s *AccountService) refreshTokenKeepaliveDueAtLocked(account Account, now time.Time) *time.Time {
	anchor := s.refreshTokenKeepaliveAnchorLocked(account)
	if anchor == nil {
		return nil
	}
	due := anchor.Add(time.Duration(refreshTokenKeepaliveSeconds) * time.Second)
	return &due
}

func (s *AccountService) recentRefreshTokenKeepaliveErrorLocked(account Account, now time.Time) bool {
	lastError := parseAccountTime(account["last_token_refresh_error_at"])
	if lastError == nil {
		return false
	}
	// 未来时间戳钳制到 now：否则 now.Sub(未来) 为负，负数恒小于退避窗口，
	// 退避永不过期 —— 账号永久被排除在 keepalive 之外。
	if lastError.After(now) {
		return true
	}
	return now.Sub(*lastError) < time.Duration(refreshKeepaliveErrorBackoff)*time.Second
}

func (s *AccountService) recentTokenRefreshErrorLocked(account Account) bool {
	lastError := parseAccountTime(account["last_token_refresh_error_at"])
	if lastError == nil {
		return false
	}
	return time.Since(*lastError) < time.Duration(tokenRefreshErrorBackoff)*time.Second
}

// newUUID 复刻 Python 的 uuid4().hex。
func newUUID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// OAuth 桥复用号池刷新的同一套凭据常量。
const (
	oauthClientIDConst  = oauthClientID
	oauthUserAgentConst = oauthUserAgent
)

// Version 返回号池内容版本号，每次账号增删改都会自增。
//
// 供 ModelCatalog 做 O(1) 的缓存失效判断，避免每次请求遍历全部账号。
func (s *AccountService) Version() uint64 {
	return s.version.Load()
}

// structuralSignature 只取「影响模型目录」的字段：账号集合 + 类型 + 状态。
//
// 为什么需要它：saveAccountsLocked 在每个请求都会跑（MarkTextUsed 更新
// last_used_at、MarkImageResult 更新 quota/success/fail）。如果每次落盘都
// 自增版本号，模型缓存就永远失效，/v1/models 每次都要重新拉上游
// （实测 63ms → 1348ms 的退化）。
//
// 模型目录只关心「有哪些类型/来源的账号、是否可用」，不关心使用计数。
func (s *AccountService) structuralSignatureLocked() string {
	type entry struct {
		token      string
		typ        string
		sourceType string
		status     string
	}
	entries := make([]entry, 0, len(s.accounts))
	for token, account := range s.accounts {
		entries = append(entries, entry{
			token:      token,
			typ:        stringValue(account["type"]),
			sourceType: stringValue(account["source_type"]),
			status:     stringValue(account["status"]),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].token < entries[j].token })

	var builder strings.Builder
	for _, item := range entries {
		builder.WriteString(item.token)
		builder.WriteByte('\x00')
		builder.WriteString(item.typ)
		builder.WriteByte('\x00')
		builder.WriteString(item.sourceType)
		builder.WriteByte('\x00')
		builder.WriteString(item.status)
		builder.WriteByte('\n')
	}
	return builder.String()
}

// bumpVersionLocked 只在「结构性内容」变化时自增版本号。
//
// 纯使用统计（last_used_at / success / fail / quota）不触发自增 ——
// 它们不影响模型目录，让它们触发只会白白打掉缓存。
func (s *AccountService) bumpVersionLocked() {
	signature := s.structuralSignatureLocked()
	if signature == s.structuralSig {
		return
	}
	s.structuralSig = signature
	s.version.Add(1)
}
