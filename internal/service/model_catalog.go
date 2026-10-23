package service

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelFetcher 由 upstream 层注入：给一个 access_token（空串=匿名），
// 返回上游模型列表。
type ModelFetcher func(ctx context.Context, accessToken string) ([]map[string]any, error)

// ModelCatalog 复刻 services/model_service.py 的 ModelCatalogService。
//
// 缓存策略：
//   - TTL 300 秒
//   - 账号集合的「类型→数量」签名变化时立即失效
//
// 这两个条件任一满足才重新拉上游。理由：模型目录是按账号类型聚合的，
// 加/删账号会改变可见模型集合。
type ModelCatalog struct {
	accounts AccountLister
	fetch    ModelFetcher

	ttl   time.Duration
	clock func() time.Time

	mu        sync.RWMutex
	expiresAt time.Time
	// cachedVersion 是上次刷新时号池的版本号（来自 VersionedLister）。
	// 有它就不必每次请求都遍历账号算签名。
	cachedVersion uint64
	signature     string
	anonymous     map[string]map[string]any
	byAccountType map[string]map[string]map[string]any
}

// AccountLister 是 ModelCatalog 对号池的最小依赖。
type AccountLister interface {
	ListAccounts() []Account
}

// VersionedLister 是可选能力：号池能给出一个 O(1) 可读的内容版本号。
//
// 有版本号时，缓存命中判断是一次原子读；没有时退回「遍历账号算签名」
// （正确但慢，269 账号约 101μs）。生产用 AccountService 实现了它。
type VersionedLister interface {
	Version() uint64
}

// NewModelCatalog 构造。fetch 为 nil 时只返回空目录（测试用）。
func NewModelCatalog(accounts AccountLister, fetch ModelFetcher) *ModelCatalog {
	return &ModelCatalog{
		accounts:      accounts,
		fetch:         fetch,
		ttl:           300 * time.Second,
		clock:         time.Now,
		anonymous:     map[string]map[string]any{},
		byAccountType: map[string]map[string]map[string]any{},
	}
}

// activeGroups 按账号类型分组，跳过禁用/异常账号（与 Python 一致）。
func (c *ModelCatalog) activeGroups() map[string][]string {
	groups := map[string][]string{}
	for _, account := range c.accounts.ListAccounts() {
		status := stringValue(account["status"])
		if status == StatusDisabled || status == StatusAbnormal {
			continue
		}
		token := strings.TrimSpace(stringValue(account["access_token"]))
		accountType := normalizeAccountType(account["type"])
		if token == "" || accountType == "" {
			continue
		}
		groups[accountType] = append(groups[accountType], token)
	}
	return groups
}

// signatureOf 把分组压成可比较的字符串（Python 用 tuple of (type, count)）。
func signatureOf(groups map[string][]string) string {
	types := make([]string, 0, len(groups))
	for accountType := range groups {
		types = append(types, accountType)
	}
	sort.Strings(types)
	parts := make([]string, 0, len(types))
	for _, accountType := range types {
		parts = append(parts, accountType+":"+itoa(len(groups[accountType])))
	}
	return strings.Join(parts, "|")
}

// modelMap 把上游 data 数组转成 id→模型 的映射。
func modelMap(models []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, item := range models {
		id := strings.TrimSpace(stringValue(item["id"]))
		if id == "" {
			continue
		}
		if _, exists := out[id]; exists {
			continue
		}
		out[id] = copyMap(item)
	}
	return out
}

// refresh 重新拉取上游目录。任何一路失败都保留旧值（与 Python 一致）。
func (c *ModelCatalog) refresh(ctx context.Context, groups map[string][]string, signature string, version uint64) {
	anonymous := c.anonymous
	if c.fetch != nil {
		if models, err := c.fetch(ctx, ""); err == nil {
			anonymous = modelMap(models)
		}
	}

	byAccountType := make(map[string]map[string]map[string]any, len(groups))
	for accountType, tokens := range groups {
		fetched := c.fetchAccountType(ctx, tokens)
		if fetched != nil {
			byAccountType[accountType] = fetched
			continue
		}
		// 失败时沿用旧值，避免目录在抖动时忽大忽小。
		if previous, ok := c.byAccountType[accountType]; ok {
			byAccountType[accountType] = previous
		}
	}

	c.mu.Lock()
	c.anonymous = anonymous
	c.byAccountType = byAccountType
	c.signature = signature
	c.cachedVersion = version
	c.expiresAt = c.clock().Add(c.ttl)
	c.mu.Unlock()
}

// fetchAccountType 依次尝试该类型下的账号，第一个成功的即采用。
func (c *ModelCatalog) fetchAccountType(ctx context.Context, tokens []string) map[string]map[string]any {
	if c.fetch == nil {
		return nil
	}
	attempted := map[string]bool{}
	for _, token := range tokens {
		if attempted[token] {
			continue
		}
		attempted[token] = true
		if models, err := c.fetch(ctx, token); err == nil {
			return modelMap(models)
		}
	}
	return nil
}

// ensure 在缓存过期或号池变化时刷新。
//
// 快速路径：TTL 未过期且号池版本号未变 → 直接返回，不碰账号列表。
// 慢速路径（无版本号能力时）：遍历账号算签名。
func (c *ModelCatalog) ensure(ctx context.Context) {
	versioned, hasVersion := c.accounts.(VersionedLister)

	c.mu.RLock()
	fresh := c.clock().Before(c.expiresAt)
	cachedVersion := c.cachedVersion
	c.mu.RUnlock()

	if fresh && hasVersion && versioned.Version() == cachedVersion {
		return
	}

	groups := c.activeGroups()
	signature := signatureOf(groups)

	if fresh && !hasVersion {
		c.mu.RLock()
		sameSignature := signature == c.signature
		c.mu.RUnlock()
		if sameSignature {
			return
		}
	}

	var version uint64
	if hasVersion {
		version = versioned.Version()
	}
	c.refresh(ctx, groups, signature, version)
}

// ListModels 返回聚合后的模型列表（去重、按 id 排序）。
func (c *ModelCatalog) ListModels(ctx context.Context) []map[string]any {
	c.ensure(ctx)

	c.mu.RLock()
	defer c.mu.RUnlock()

	union := make(map[string]map[string]any, len(c.anonymous))
	for id, item := range c.anonymous {
		union[id] = copyMap(item)
	}
	types := make([]string, 0, len(c.byAccountType))
	for accountType := range c.byAccountType {
		types = append(types, accountType)
	}
	sort.Strings(types)
	for _, accountType := range types {
		for id, item := range c.byAccountType[accountType] {
			if _, exists := union[id]; exists {
				continue
			}
			union[id] = copyMap(item)
		}
	}

	ids := make([]string, 0, len(union))
	for id := range union {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, union[id])
	}
	return out
}

// RouteForModel 复刻 route_for_model：返回能访问该模型的账号类型集合。
//
// allowAnonymous 为 true 表示匿名账号也能用（此时可以不选号池账号）。
func (c *ModelCatalog) RouteForModel(ctx context.Context, model string) (accountTypes map[string]bool, allowAnonymous bool) {
	c.ensure(ctx)
	name := strings.TrimSpace(model)

	c.mu.RLock()
	defer c.mu.RUnlock()

	accountTypes = map[string]bool{}
	for accountType, models := range c.byAccountType {
		if _, ok := models[name]; ok {
			accountTypes[accountType] = true
		}
	}
	_, allowAnonymous = c.anonymous[name]
	return accountTypes, allowAnonymous
}

// AccountSummaries 把号池转成 protocol 需要的摘要（含 source_type）。
func AccountSummaries(accounts []Account) []AccountSummaryLike {
	out := make([]AccountSummaryLike, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, AccountSummaryLike{
			Type:       normalizeAccountType(account["type"]),
			SourceType: normalizeSourceType(account["source_type"]),
		})
	}
	return out
}

// AccountSummaryLike 是 protocol.AccountSummary 的结构对等体，
// 避免 service 直接依赖 protocol 包。
type AccountSummaryLike struct {
	Type       string
	SourceType string
}
