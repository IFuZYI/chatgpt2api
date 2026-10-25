package service

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/config"
)

// normalizeProxyURL 复刻 Python 的 normalize_proxy_url：
// socks:// 与 socks5:// 一律升级成 socks5h://（远程 DNS）。
func normalizeProxyURL(raw string) string {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return ""
	}
	if !strings.Contains(candidate, "://") {
		candidate = colonProxyToURL(candidate)
	}
	lowered := strings.ToLower(candidate)
	switch {
	case strings.HasPrefix(lowered, "socks://"):
		return "socks5h://" + candidate[len("socks://"):]
	case strings.HasPrefix(lowered, "socks5://"):
		return "socks5h://" + candidate[len("socks5://"):]
	default:
		return candidate
	}
}

// colonProxyToURL 支持 host:port:user:pass 与 host:port 两种简写。
func colonProxyToURL(raw string) string {
	parts := strings.SplitN(raw, ":", 4)
	if len(parts) == 4 && isDigits(parts[1]) {
		return fmt.Sprintf("http://%s:%s@%s:%s",
			url.QueryEscape(parts[2]), url.QueryEscape(parts[3]), parts[0], parts[1])
	}
	if len(parts) == 2 && isDigits(parts[1]) {
		return "http://" + raw
	}
	return raw
}

func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, ch := range text {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// ProxyRuntimeProfile 是一次请求的出站代理决策结果。
type ProxyRuntimeProfile struct {
	ProxyURL                string
	ProxySource             string
	Resource                bool
	RuntimeEnabled          bool
	EgressMode              string
	SkipSSLVerify           bool
	ResetSessionStatusCodes []int
	Clearance               map[string]any
}

// ClearanceEnabled 复刻 Python 的 property：
// 运行时开 + clearance.enabled + mode 是 manual/flaresolverr。
func (p ProxyRuntimeProfile) ClearanceEnabled() bool {
	if !p.RuntimeEnabled {
		return false
	}
	if !truthy(p.Clearance["enabled"]) {
		return false
	}
	switch p.ClearanceMode() {
	case "manual", "flaresolverr":
		return true
	default:
		return false
	}
}

func (p ProxyRuntimeProfile) ClearanceMode() string {
	return strings.ToLower(strings.TrimSpace(stringValue(p.Clearance["mode"])))
}

func (p ProxyRuntimeProfile) RefreshInterval() int {
	return maxInt(intValue(p.Clearance["refresh_interval"]), 0)
}

func (p ProxyRuntimeProfile) TimeoutSec() int {
	timeout := intValue(p.Clearance["timeout_sec"])
	if timeout <= 0 {
		return 60
	}
	return timeout
}

// ClearanceBundle 是一次 Cloudflare 清障的结果。
type ClearanceBundle struct {
	TargetHost string
	ProxyURL   string
	Cookies    map[string]string
	UserAgent  string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
}

// IsValidFor 复刻 Python 的 is_valid_for：host 与 proxy 都要匹配且未过期。
func (b *ClearanceBundle) IsValidFor(targetHost, proxyURL string) bool {
	host := normalizeHost(targetHost)
	if b.TargetHost != "" && host != "" && normalizeHost(b.TargetHost) != host {
		return false
	}
	if normalizeProxyURL(b.ProxyURL) != normalizeProxyURL(proxyURL) {
		return false
	}
	if b.ExpiresAt != nil && !time.Now().Before(*b.ExpiresAt) {
		return false
	}
	return len(b.Cookies) > 0 || b.UserAgent != ""
}

// CookieHeader 拼成 Cookie 头。
func (b *ClearanceBundle) CookieHeader() string {
	return cookiesToHeader(b.Cookies)
}

// ProxySettings 是全局代理配置的运行时视图，负责优先级链与 clearance 缓存。
//
// 优先级（高到低）：账号级代理 → 稳定代理运行时 → 显式代理 → 全局代理。
type ProxySettings struct {
	cfg *config.Store

	mu             sync.RWMutex
	clearanceCache map[string]*ClearanceBundle
	flightLocks    map[string]*sync.Mutex
}

func NewProxySettings(cfg *config.Store) *ProxySettings {
	return &ProxySettings{
		cfg:            cfg,
		clearanceCache: map[string]*ClearanceBundle{},
		flightLocks:    map[string]*sync.Mutex{},
	}
}

// GetProfile 复刻 Python 的 get_profile。
//
// upstream=false 时（例如访问邮箱、CPA 等辅助链路）不套用稳定代理运行时，
// 只有账号级/显式/全局代理生效——这是刻意的：WARP 只接管上游。
func (s *ProxySettings) GetProfile(account map[string]any, explicitProxy string, resource, upstream bool) ProxyRuntimeProfile {
	runtime := s.cfg.ProxyRuntimeSettings()
	clearance, _ := runtime["clearance"].(map[string]any)
	if clearance == nil {
		clearance = map[string]any{}
	}
	runtimeEnabled := truthy(runtime["enabled"])
	egressMode := strings.ToLower(strings.TrimSpace(stringValue(runtime["egress_mode"])))
	if egressMode == "" {
		egressMode = "direct"
	}

	accountProxy := ""
	if account != nil {
		accountProxy = strings.TrimSpace(stringValue(account["proxy"]))
	}
	explicit := strings.TrimSpace(explicitProxy)
	legacy := s.cfg.Proxy()

	runtimeProxy := ""
	runtimeProxySource := "runtime"
	if upstream && runtimeEnabled && egressMode == "single_proxy" {
		resourceProxy := ""
		if resource {
			resourceProxy = strings.TrimSpace(stringValue(runtime["resource_proxy_url"]))
		}
		if resourceProxy != "" {
			runtimeProxy = resourceProxy
			runtimeProxySource = "runtime_resource"
		} else {
			runtimeProxy = strings.TrimSpace(stringValue(runtime["proxy_url"]))
		}
	}

	selected := ""
	source := "direct"
	switch {
	case accountProxy != "":
		selected, source = accountProxy, "account"
	case runtimeProxy != "":
		selected, source = runtimeProxy, runtimeProxySource
	case explicit != "":
		selected, source = explicit, "explicit"
	case legacy != "":
		selected, source = legacy, "global"
	}

	skipSSL := false
	if runtimeEnabled {
		skipSSL = truthy(runtime["skip_ssl_verify"])
	}

	return ProxyRuntimeProfile{
		ProxyURL:                normalizeProxyURL(selected),
		ProxySource:             source,
		Resource:                resource,
		RuntimeEnabled:          runtimeEnabled,
		EgressMode:              egressMode,
		SkipSSLVerify:           skipSSL,
		ResetSessionStatusCodes: statusCodes(runtime["reset_session_status_codes"]),
		Clearance:               clearance,
	}
}

func statusCodes(value any) []int {
	items, ok := value.([]any)
	if !ok {
		return []int{403}
	}
	out := make([]int, 0, len(items))
	for _, item := range items {
		code := intValue(item)
		if code < 100 || code > 599 {
			continue
		}
		duplicate := false
		for _, existing := range out {
			if existing == code {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, code)
		}
	}
	if len(out) == 0 {
		return []int{403}
	}
	return out
}

// BuildHeaders 把 clearance 的 cookie 与 UA 合进请求头。
func (s *ProxySettings) BuildHeaders(headers map[string]string, targetURL string, profile ProxyRuntimeProfile) map[string]string {
	merged := make(map[string]string, len(headers))
	for key, value := range headers {
		merged[key] = value
	}
	if !profile.ClearanceEnabled() {
		return merged
	}
	targetHost := hostFromURL(targetURL)
	bundle := s.bundleForHeaders(profile, targetHost)
	if bundle == nil || !bundle.IsValidFor(targetHost, profile.ProxyURL) {
		return merged
	}
	if bundle.UserAgent != "" && findHeaderKey(merged, "user-agent") == "" {
		merged["User-Agent"] = bundle.UserAgent
	}
	if len(bundle.Cookies) > 0 {
		cookieKey := findHeaderKey(merged, "cookie")
		if cookieKey == "" {
			cookieKey = "Cookie"
		}
		if header := mergeCookieHeader(merged[cookieKey], bundle.Cookies); header != "" {
			merged[cookieKey] = header
		}
	}
	return merged
}

// RuntimeStatus 复刻 get_runtime_status，键名是 /health 与设置页的契约。
func (s *ProxySettings) RuntimeStatus() map[string]any {
	profile := s.GetProfile(nil, "", false, true)

	s.mu.RLock()
	hosts := make([]string, 0, len(s.clearanceCache))
	for _, bundle := range s.clearanceCache {
		if bundle.TargetHost != "" {
			hosts = append(hosts, bundle.TargetHost)
		}
	}
	cachedCount := len(s.clearanceCache)
	s.mu.RUnlock()

	sort.Strings(hosts)
	uniqueHosts := make([]any, 0, len(hosts))
	seen := map[string]bool{}
	for _, host := range hosts {
		if seen[host] {
			continue
		}
		seen[host] = true
		uniqueHosts = append(uniqueHosts, host)
	}

	return map[string]any{
		"enabled":                profile.RuntimeEnabled,
		"egress_mode":            profile.EgressMode,
		"proxy_source":           profile.ProxySource,
		"has_proxy":              profile.ProxyURL != "",
		"clearance_enabled":      profile.ClearanceEnabled(),
		"clearance_mode":         profile.ClearanceMode(),
		"has_clearance_bundle":   cachedCount > 0,
		"cached_clearance_hosts": uniqueHosts,
	}
}

// RefreshClearance 手动/FlareSolverr 清障。mode=none 时直接返回 nil。
func (s *ProxySettings) RefreshClearance(targetURL string, profile ProxyRuntimeProfile, force bool) *ClearanceBundle {
	if !profile.ClearanceEnabled() {
		return nil
	}
	if targetURL == "" {
		targetURL = "https://chatgpt.com"
	}
	targetHost := hostFromURL(targetURL)
	key := clearanceKey(profile.ProxyURL, targetHost)

	if profile.ClearanceMode() == "manual" {
		bundle := buildManualBundle(profile, targetHost)
		if bundle != nil {
			s.setCached(key, bundle)
		}
		return bundle
	}
	if profile.ClearanceMode() != "flaresolverr" {
		return nil
	}

	cachedBefore := s.getCached(key)
	if cachedBefore != nil && !force && cachedBefore.IsValidFor(targetHost, profile.ProxyURL) {
		return cachedBefore
	}

	lock := s.flightLock(key)
	if !lock.TryLock() {
		lock.Lock()
		lock.Unlock()
		if current := s.getCached(key); current != nil {
			return current
		}
		return cachedBefore
	}
	defer lock.Unlock()

	if cachedNow := s.getCached(key); cachedNow != nil && !force && cachedNow.IsValidFor(targetHost, profile.ProxyURL) {
		return cachedNow
	}

	flaresolverrURL := strings.TrimSpace(stringValue(profile.Clearance["flaresolverr_url"]))
	bundle := fetchFlareSolverrClearance(flaresolverrURL, targetURL, profile.ProxyURL, profile.TimeoutSec())
	if bundle == nil {
		if cachedNow := s.getCached(key); cachedNow != nil {
			return cachedNow
		}
		return cachedBefore
	}
	if interval := profile.RefreshInterval(); interval > 0 {
		expires := time.Now().Add(time.Duration(interval) * time.Second)
		bundle.ExpiresAt = &expires
	}
	if bundle.TargetHost == "" {
		bundle.TargetHost = targetHost
	}
	bundle.ProxyURL = profile.ProxyURL
	s.setCached(key, bundle)
	return bundle
}

// InvalidateClearance 清掉某 host 的缓存（上游返回 403 时调用）。
func (s *ProxySettings) InvalidateClearance(targetURL string, profile ProxyRuntimeProfile) {
	key := clearanceKey(profile.ProxyURL, hostFromURL(targetURL))
	s.mu.Lock()
	delete(s.clearanceCache, key)
	s.mu.Unlock()
}

func (s *ProxySettings) bundleForHeaders(profile ProxyRuntimeProfile, targetHost string) *ClearanceBundle {
	if bundle := s.getCached(clearanceKey(profile.ProxyURL, targetHost)); bundle != nil {
		return bundle
	}
	return s.RefreshClearance("https://"+targetHost, profile, false)
}

func (s *ProxySettings) getCached(key string) *ClearanceBundle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clearanceCache[key]
}

func (s *ProxySettings) setCached(key string, bundle *ClearanceBundle) {
	s.mu.Lock()
	s.clearanceCache[key] = bundle
	s.mu.Unlock()
}

func (s *ProxySettings) flightLock(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.flightLocks[key]; ok {
		return existing
	}
	created := &sync.Mutex{}
	s.flightLocks[key] = created
	return created
}

func clearanceKey(proxyURL, targetHost string) string {
	return normalizeProxyURL(proxyURL) + "|" + normalizeHost(targetHost)
}

// buildManualBundle 用配置里粘贴的 cf_cookies / cf_clearance 组一个 bundle。
func buildManualBundle(profile ProxyRuntimeProfile, targetHost string) *ClearanceBundle {
	cookies := parseCookieHeader(stringValue(profile.Clearance["cf_cookies"]))
	cfClearance := strings.TrimSpace(stringValue(profile.Clearance["cf_clearance"]))
	if cfClearance != "" {
		cookies["cf_clearance"] = cfClearance
	}
	userAgent := strings.TrimSpace(stringValue(profile.Clearance["user_agent"]))
	if len(cookies) == 0 && userAgent == "" {
		return nil
	}
	return &ClearanceBundle{
		TargetHost: targetHost,
		ProxyURL:   profile.ProxyURL,
		Cookies:    cookies,
		UserAgent:  userAgent,
		CreatedAt:  time.Now(),
	}
}

func normalizeHost(host string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(host), "."))
}

func hostFromURL(raw string) string {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return ""
	}
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return ""
	}
	return normalizeHost(parsed.Hostname())
}

func domainMatches(host, domain string) bool {
	normalizedHost := normalizeHost(host)
	normalizedDomain := normalizeHost(strings.TrimPrefix(domain, "."))
	if normalizedDomain == "" {
		return true
	}
	return normalizedHost == normalizedDomain || strings.HasSuffix(normalizedHost, "."+normalizedDomain)
}

func parseCookieHeader(header string) map[string]string {
	cookies := map[string]string{}
	for _, part := range strings.Split(header, ";") {
		name, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found || strings.TrimSpace(name) == "" {
			continue
		}
		cookies[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return cookies
}

func cookiesToHeader(cookies map[string]string) string {
	keys := make([]string, 0, len(cookies))
	for name := range cookies {
		if name != "" {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, name := range keys {
		parts = append(parts, name+"="+cookies[name])
	}
	return strings.Join(parts, "; ")
}

func mergeCookieHeader(existing string, cookies map[string]string) string {
	existing = strings.TrimSpace(existing)
	existingNames := map[string]bool{}
	for name := range parseCookieHeader(existing) {
		existingNames[name] = true
	}
	keys := make([]string, 0, len(cookies))
	for name := range cookies {
		if name != "" && !existingNames[name] {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	additions := make([]string, 0, len(keys))
	for _, name := range keys {
		additions = append(additions, name+"="+cookies[name])
	}
	switch {
	case existing != "" && len(additions) > 0:
		return strings.TrimRight(existing, "; ") + "; " + strings.Join(additions, "; ")
	case existing != "":
		return existing
	default:
		return strings.Join(additions, "; ")
	}
}

func findHeaderKey(headers map[string]string, name string) string {
	target := strings.ToLower(name)
	for key := range headers {
		if strings.ToLower(key) == target {
			return key
		}
	}
	return ""
}

var urlCredentialPattern = regexp.MustCompile(`((?:https?|socks5h?|socks)://)([^\s/@:]+):([^\s/@]+)@`)

// redactURLCredentials 把代理 URL 里的账号密码打码，用于错误信息与日志。
func redactURLCredentials(text string) string {
	return urlCredentialPattern.ReplaceAllString(text, "$1[REDACTED]@")
}
