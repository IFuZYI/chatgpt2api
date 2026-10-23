package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Sub2APIConfig 复刻 services/sub2api_service.py 的 Sub2APIConfig：
// 管理 sub2api 服务器连接（支持 email+password 或 api_key 两种鉴权）。
type Sub2APIConfig struct {
	mu      sync.Mutex
	path    string
	servers []map[string]any

	// tokenCache 缓存每个 server 的 JWT（避免每次列表都重新登录）。
	tokenMu    sync.Mutex
	tokenCache map[string]cachedToken
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

const tokenRefreshSkew = 5 * 60 // 提前 5 分钟刷新

// NewSub2APIConfig 从 data/sub2api_config.json 加载。
func NewSub2APIConfig(dataDir string) *Sub2APIConfig {
	cfg := &Sub2APIConfig{
		path:       filepath.Join(dataDir, "sub2api_config.json"),
		tokenCache: map[string]cachedToken{},
	}
	cfg.servers = cfg.load()
	return cfg
}

func (c *Sub2APIConfig) load() []map[string]any {
	payload, err := os.ReadFile(c.path)
	if err != nil {
		return nil
	}
	var list []map[string]any
	if err := json.Unmarshal(payload, &list); err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if item != nil {
			out = append(out, normalizeSub2APIServer(item, true))
		}
	}
	return out
}

func (c *Sub2APIConfig) save() {
	payload, err := json.MarshalIndent(c.servers, "", "  ")
	if err != nil {
		return
	}
	payload = append(payload, '\n')
	_ = os.MkdirAll(filepath.Dir(c.path), 0o755)
	_ = os.WriteFile(c.path, payload, 0o644)
}

// normalizeSub2APIServer 复刻 _normalize_server。
func normalizeSub2APIServer(raw map[string]any, failUnfinished bool) map[string]any {
	serverID := trimSpaceValue(raw["id"])
	if serverID == "" {
		serverID = newShortID()
	}
	return map[string]any{
		"id":         serverID,
		"name":       trimSpaceValue(raw["name"]),
		"base_url":   trimSpaceValue(raw["base_url"]),
		"email":      trimSpaceValue(raw["email"]),
		"password":   trimSpaceValue(raw["password"]),
		"api_key":    trimSpaceValue(raw["api_key"]),
		"group_id":   trimSpaceValue(raw["group_id"]),
		"import_job": normalizeImportJob(raw["import_job"], failUnfinished),
	}
}

func (c *Sub2APIConfig) ListServers() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, 0, len(c.servers))
	for _, server := range c.servers {
		out = append(out, copyMap(server))
	}
	return out
}

func (c *Sub2APIConfig) GetServer(serverID string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, server := range c.servers {
		if stringValue(server["id"]) == serverID {
			return copyMap(server)
		}
	}
	return nil
}

func (c *Sub2APIConfig) AddServer(name, baseURL, email, password, apiKey, groupID string) map[string]any {
	server := normalizeSub2APIServer(map[string]any{
		"id": newShortID(), "name": name, "base_url": baseURL,
		"email": email, "password": password, "api_key": apiKey, "group_id": groupID,
	}, true)
	c.mu.Lock()
	c.servers = append(c.servers, server)
	c.save()
	c.mu.Unlock()
	return copyMap(server)
}

func (c *Sub2APIConfig) UpdateServer(serverID string, updates map[string]any) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for index, server := range c.servers {
		if stringValue(server["id"]) != serverID {
			continue
		}
		merged := copyMap(server)
		for key, value := range updates {
			if value != nil {
				merged[key] = value
			}
		}
		merged["id"] = serverID
		c.servers[index] = normalizeSub2APIServer(merged, true)
		c.save()
		return copyMap(c.servers[index])
	}
	return nil
}

func (c *Sub2APIConfig) DeleteServer(serverID string) bool {
	c.mu.Lock()
	kept := make([]map[string]any, 0, len(c.servers))
	for _, server := range c.servers {
		if stringValue(server["id"]) == serverID {
			continue
		}
		kept = append(kept, server)
	}
	removed := len(kept) < len(c.servers)
	if removed {
		c.servers = kept
		c.save()
	}
	c.mu.Unlock()

	if removed {
		c.tokenMu.Lock()
		delete(c.tokenCache, serverID)
		c.tokenMu.Unlock()
	}
	return removed
}

func (c *Sub2APIConfig) SetImportJob(serverID string, job any) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for index, server := range c.servers {
		if stringValue(server["id"]) != serverID {
			continue
		}
		next := copyMap(server)
		next["import_job"] = normalizeImportJob(job, false)
		c.servers[index] = next
		c.save()
		return copyMap(next)
	}
	return nil
}

func (c *Sub2APIConfig) GetImportJob(serverID string) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, server := range c.servers {
		if stringValue(server["id"]) == serverID {
			return server["import_job"]
		}
	}
	return nil
}

// authHeaders 复刻 _auth_headers：api_key 优先，否则用 email+password 登录换 JWT。
func (c *Sub2APIConfig) authHeaders(ctx CtxLike, server map[string]any) (map[string]string, error) {
	if apiKey := trimSpaceValue(server["api_key"]); apiKey != "" {
		return map[string]string{"x-api-key": apiKey, "Accept": "application/json"}, nil
	}

	email := trimSpaceValue(server["email"])
	password := trimSpaceValue(server["password"])
	if email == "" || password == "" {
		return nil, errors.New("sub2api server requires email+password or api_key")
	}

	serverID := trimSpaceValue(server["id"])
	baseURL := trimSpaceValue(server["base_url"])

	c.tokenMu.Lock()
	cached, hasCached := c.tokenCache[serverID]
	c.tokenMu.Unlock()
	if hasCached && cached.expiresAt.After(time.Now()) {
		return map[string]string{
			"Authorization": "Bearer " + cached.token,
			"Accept":        "application/json",
		}, nil
	}

	token, expiresAt, err := sub2apiLogin(ctx, baseURL, email, password)
	if err != nil {
		return nil, err
	}
	c.tokenMu.Lock()
	c.tokenCache[serverID] = cachedToken{token: token, expiresAt: expiresAt}
	c.tokenMu.Unlock()
	return map[string]string{
		"Authorization": "Bearer " + token,
		"Accept":        "application/json",
	}, nil
}

// sub2apiLogin 复刻 _login：POST /api/v1/auth/login 换 access_token。
func sub2apiLogin(ctx CtxLike, baseURL, email, password string) (string, time.Time, error) {
	payload, err := json.Marshal(map[string]any{"email": email, "password": password})
	if err != nil {
		return "", time.Time{}, err
	}
	target := trimRightSlash(baseURL) + "/api/v1/auth/login"
	body, status, err := httpPostJSON(ctx, target, map[string]string{"Accept": "application/json"}, payload, 30*time.Second)
	if err != nil {
		return "", time.Time{}, err
	}
	if status >= 400 {
		return "", time.Time{}, fmt.Errorf("sub2api login failed: HTTP %d", status)
	}

	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", time.Time{}, errors.New("sub2api login payload is invalid")
	}
	inner := unwrapEnvelope(raw)
	obj, ok := inner.(map[string]any)
	if !ok {
		return "", time.Time{}, errors.New("sub2api login payload is invalid")
	}

	token := trimSpaceValue(obj["access_token"])
	if token == "" {
		return "", time.Time{}, errors.New("sub2api login did not return access_token")
	}
	expiresIn := intValue(obj["expires_in"])
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	lifetime := expiresIn
	if lifetime < 60 {
		lifetime = 60
	}
	expiresAt := time.Now().Add(time.Duration(lifetime-tokenRefreshSkew) * time.Second)
	return token, expiresAt, nil
}

// unwrapEnvelope 剥掉 sub2api 的 {code, message, data} 外壳。
func unwrapEnvelope(payload any) any {
	if obj, ok := payload.(map[string]any); ok {
		if _, hasData := obj["data"]; hasData {
			if _, hasCode := obj["code"]; hasCode {
				return obj["data"]
			}
		}
	}
	return payload
}

// extractPagedItems 从分页响应里取 (items, total)。
func extractPagedItems(payload any) ([]any, int) {
	inner := unwrapEnvelope(payload)
	if list, ok := inner.([]any); ok {
		return list, len(list)
	}
	if obj, ok := inner.(map[string]any); ok {
		for _, key := range []string{"items", "data", "list"} {
			if list, ok := obj[key].([]any); ok {
				total := intValue(obj["total"])
				if total == 0 {
					total = len(list)
				}
				return list, total
			}
		}
	}
	return nil, 0
}

// Sub2APIAccount 是远程账号摘要。
type Sub2APIAccount struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Email           string `json:"email"`
	PlanType        string `json:"plan_type"`
	Status          string `json:"status"`
	ExpiresAt       string `json:"expires_at"`
	HasRefreshToken bool   `json:"has_refresh_token"`
}

// Sub2APIGroup 是远程分组摘要。
type Sub2APIGroup struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	Platform           string `json:"platform"`
	Status             string `json:"status"`
	AccountCount       int    `json:"account_count"`
	ActiveAccountCount int    `json:"active_account_count"`
}

// ListRemoteAccounts 复刻 list_remote_accounts：分页拉 openai/oauth 账号。
func (c *Sub2APIConfig) ListRemoteAccounts(ctx CtxLike, server map[string]any) ([]Sub2APIAccount, error) {
	baseURL := trimSpaceValue(server["base_url"])
	if baseURL == "" {
		return []Sub2APIAccount{}, nil
	}
	headers, err := c.authHeaders(ctx, server)
	if err != nil {
		return nil, err
	}
	groupID := trimSpaceValue(server["group_id"])

	items := make([]Sub2APIAccount, 0, 32)
	for page := 1; page <= 50; page++ {
		query := fmt.Sprintf("?platform=openai&type=oauth&page=%d&page_size=200", page)
		if groupID != "" {
			query += "&group=" + urlEncode(groupID)
		}
		body, status, err := httpGetJSON(ctx, trimRightSlash(baseURL)+"/api/v1/admin/accounts"+query, headers, 30*time.Second)
		if err != nil {
			return nil, err
		}
		if status >= 400 {
			return nil, fmt.Errorf("sub2api list failed: HTTP %d", status)
		}

		var payload any
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		data, total := extractPagedItems(payload)
		if len(data) == 0 {
			break
		}
		for _, entry := range data {
			account, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			credentials, _ := account["credentials"].(map[string]any)
			accountID := account["id"]
			if accountID == nil {
				continue
			}
			name := trimSpaceValue(account["name"])
			email := trimSpaceValue(credentials["email"])
			if email == "" {
				email = name
			}
			items = append(items, Sub2APIAccount{
				ID:              stringValue(accountID),
				Name:            name,
				Email:           email,
				PlanType:        trimSpaceValue(credentials["plan_type"]),
				Status:          trimSpaceValue(account["status"]),
				ExpiresAt:       trimSpaceValue(credentials["expires_at"]),
				HasRefreshToken: trimSpaceValue(credentials["refresh_token"]) != "",
			})
		}
		if page*200 >= total || len(data) < 200 {
			break
		}
	}
	return items, nil
}

// ListRemoteGroups 复刻 list_remote_groups。
func (c *Sub2APIConfig) ListRemoteGroups(ctx CtxLike, server map[string]any) ([]Sub2APIGroup, error) {
	baseURL := trimSpaceValue(server["base_url"])
	if baseURL == "" {
		return []Sub2APIGroup{}, nil
	}
	headers, err := c.authHeaders(ctx, server)
	if err != nil {
		return nil, err
	}

	items := make([]Sub2APIGroup, 0, 16)
	for page := 1; page <= 50; page++ {
		query := fmt.Sprintf("?page=%d&page_size=200", page)
		body, status, err := httpGetJSON(ctx, trimRightSlash(baseURL)+"/api/v1/admin/groups"+query, headers, 30*time.Second)
		if err != nil {
			return nil, err
		}
		if status >= 400 {
			return nil, fmt.Errorf("sub2api groups failed: HTTP %d", status)
		}
		var payload any
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		data, total := extractPagedItems(payload)
		if len(data) == 0 {
			break
		}
		for _, entry := range data {
			group, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			groupID := group["id"]
			if groupID == nil {
				continue
			}
			items = append(items, Sub2APIGroup{
				ID:                 stringValue(groupID),
				Name:               trimSpaceValue(group["name"]),
				Description:        trimSpaceValue(group["description"]),
				Platform:           trimSpaceValue(group["platform"]),
				Status:             trimSpaceValue(group["status"]),
				AccountCount:       intValue(group["account_count"]),
				ActiveAccountCount: intValue(group["active_account_count"]),
			})
		}
		if page*200 >= total || len(data) < 200 {
			break
		}
	}
	return items, nil
}

// FetchAccessTokens 复刻 _fetch_access_tokens_for_accounts：
// 拉取选中账号的 access_token，返回 (tokens, errors)。
func (c *Sub2APIConfig) FetchAccessTokens(ctx CtxLike, server map[string]any, accountIDs []string) ([]string, []map[string]any, error) {
	baseURL := trimSpaceValue(server["base_url"])
	headers, err := c.authHeaders(ctx, server)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, 0, len(accountIDs))
	for _, item := range accountIDs {
		if trimmed := trimSpaceString(item); trimmed != "" {
			ids = append(ids, trimmed)
		}
	}
	if len(ids) == 0 {
		return []string{}, []map[string]any{}, nil
	}

	query := "?ids=" + urlEncode(strings.Join(ids, ",")) + "&timezone=" + urlEncode("Asia/Shanghai")
	body, status, err := httpGetJSON(ctx, trimRightSlash(baseURL)+"/api/v1/admin/accounts/data"+query, headers, 30*time.Second)
	if err != nil {
		return nil, nil, err
	}
	if status >= 400 {
		return nil, nil, fmt.Errorf("HTTP %d", status)
	}

	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, err
	}
	inner := unwrapEnvelope(payload)
	obj, _ := inner.(map[string]any)
	accounts, _ := obj["accounts"].([]any)
	if accounts == nil {
		return nil, nil, errors.New("invalid export payload")
	}

	tokens := make([]string, 0, len(accounts))
	failures := make([]map[string]any, 0)
	for _, entry := range accounts {
		account, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		credentials, _ := account["credentials"].(map[string]any)
		token := extractAccessToken(credentials)
		if token != "" {
			tokens = append(tokens, token)
			continue
		}
		name := trimSpaceValue(account["id"])
		if name == "" {
			name = trimSpaceValue(credentials["chatgpt_account_id"])
		}
		if name == "" {
			name = trimSpaceValue(account["name"])
		}
		if name == "" {
			name = "unknown"
		}
		failures = append(failures, map[string]any{"name": name, "error": "missing access_token"})
	}

	if len(accounts) < len(ids) {
		failures = append(failures, map[string]any{
			"name":  strings.Join(ids, ","),
			"error": fmt.Sprintf("exported %d/%d accounts", len(accounts), len(ids)),
		})
	}
	return tokens, failures, nil
}

func extractAccessToken(credentials map[string]any) string {
	if credentials == nil {
		return ""
	}
	for _, key := range []string{"access_token", "accessToken", "token"} {
		if value := trimSpaceValue(credentials[key]); value != "" {
			return value
		}
	}
	return ""
}

// SanitizeSub2APIServer 复刻 sanitize_sub2api_server：剔除密码与 api_key，只留 has_api_key 标记。
func SanitizeSub2APIServer(server map[string]any) map[string]any {
	if server == nil {
		return nil
	}
	out := make(map[string]any, len(server))
	for key, value := range server {
		if key == "password" || key == "api_key" {
			continue
		}
		out[key] = value
	}
	out["has_api_key"] = trimSpaceValue(server["api_key"]) != ""
	return out
}

// SanitizeSub2APIServers 批量清洗。
func SanitizeSub2APIServers(servers []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(servers))
	for _, server := range servers {
		if sanitized := SanitizeSub2APIServer(server); sanitized != nil {
			out = append(out, sanitized)
		}
	}
	return out
}
