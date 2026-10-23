package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CPAConfig 复刻 services/cpa_service.py 的 CPAConfig：
// 管理 CPA 服务器连接（名字 + base_url + secret_key）。
//
// 存储格式兼容两种历史形态：
//   - 单个对象 {"base_url": ...}（老版本）
//   - 数组 [{...}, {...}]
type CPAConfig struct {
	mu    sync.Mutex
	path  string
	pools []map[string]any
}

// NewCPAConfig 从 data/cpa_config.json 加载。
func NewCPAConfig(dataDir string) *CPAConfig {
	cfg := &CPAConfig{path: filepath.Join(dataDir, "cpa_config.json")}
	cfg.pools = cfg.load()
	return cfg
}

func (c *CPAConfig) load() []map[string]any {
	payload, err := os.ReadFile(c.path)
	if err != nil {
		return nil
	}
	// 老格式：单个对象
	var single map[string]any
	if err := json.Unmarshal(payload, &single); err == nil && single != nil {
		if _, ok := single["base_url"]; ok {
			pool := normalizeCPAPool(single, true)
			if stringValue(pool["base_url"]) != "" {
				return []map[string]any{pool}
			}
			return nil
		}
	}
	var list []map[string]any
	if err := json.Unmarshal(payload, &list); err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if item != nil {
			out = append(out, normalizeCPAPool(item, true))
		}
	}
	return out
}

func (c *CPAConfig) save() {
	payload, err := json.MarshalIndent(c.pools, "", "  ")
	if err != nil {
		return
	}
	payload = append(payload, '\n')
	_ = os.MkdirAll(filepath.Dir(c.path), 0o755)
	_ = os.WriteFile(c.path, payload, 0o644)
}

// normalizeCPAPool 复刻 _normalize_pool。
// failUnfinished=true 时把 pending/running 的导入任务标记成 failed
// （服务重启后不可能还有正在跑的导入）。
func normalizeCPAPool(raw map[string]any, failUnfinished bool) map[string]any {
	poolID := stringValue(raw["id"])
	if poolID == "" {
		poolID = newShortID()
	}
	return map[string]any{
		"id":         poolID,
		"name":       trimSpaceValue(raw["name"]),
		"base_url":   trimSpaceValue(raw["base_url"]),
		"secret_key": trimSpaceValue(raw["secret_key"]),
		"import_job": normalizeImportJob(raw["import_job"], failUnfinished),
	}
}

// normalizeImportJob 复刻 _normalize_import_job。
func normalizeImportJob(raw any, failUnfinished bool) any {
	item, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	status := trimSpaceValue(item["status"])
	if status == "" {
		status = "failed"
	}
	if failUnfinished && (status == "pending" || status == "running") {
		status = "failed"
	}
	jobID := trimSpaceValue(item["job_id"])
	if jobID == "" {
		jobID = newShortID()
	}
	createdAt := trimSpaceValue(item["created_at"])
	if createdAt == "" {
		createdAt = nowISOValue()
	}
	updatedAt := trimSpaceValue(item["updated_at"])
	if updatedAt == "" {
		updatedAt = createdAt
	}
	errorsList, _ := item["errors"].([]any)
	if errorsList == nil {
		errorsList = []any{}
	}
	return map[string]any{
		"job_id":     jobID,
		"status":     status,
		"created_at": createdAt,
		"updated_at": updatedAt,
		"total":      intValue(item["total"]),
		"completed":  intValue(item["completed"]),
		"added":      intValue(item["added"]),
		"skipped":    intValue(item["skipped"]),
		"refreshed":  intValue(item["refreshed"]),
		"failed":     intValue(item["failed"]),
		"errors":     errorsList,
	}
}

func (c *CPAConfig) ListPools() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, 0, len(c.pools))
	for _, pool := range c.pools {
		out = append(out, copyMap(pool))
	}
	return out
}

func (c *CPAConfig) GetPool(poolID string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, pool := range c.pools {
		if stringValue(pool["id"]) == poolID {
			return copyMap(pool)
		}
	}
	return nil
}

func (c *CPAConfig) AddPool(name, baseURL, secretKey string) map[string]any {
	pool := normalizeCPAPool(map[string]any{
		"id": newShortID(), "name": name, "base_url": baseURL, "secret_key": secretKey,
	}, true)
	c.mu.Lock()
	c.pools = append(c.pools, pool)
	c.save()
	c.mu.Unlock()
	return copyMap(pool)
}

// UpdatePool 的 updates 用 nil 表示未提供（与 Pydantic 的 exclude_none 对齐）。
func (c *CPAConfig) UpdatePool(poolID string, updates map[string]any) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for index, pool := range c.pools {
		if stringValue(pool["id"]) != poolID {
			continue
		}
		merged := copyMap(pool)
		for key, value := range updates {
			if value != nil {
				merged[key] = value
			}
		}
		merged["id"] = poolID
		c.pools[index] = normalizeCPAPool(merged, true)
		c.save()
		return copyMap(c.pools[index])
	}
	return nil
}

func (c *CPAConfig) DeletePool(poolID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := len(c.pools)
	kept := make([]map[string]any, 0, len(c.pools))
	for _, pool := range c.pools {
		if stringValue(pool["id"]) == poolID {
			continue
		}
		kept = append(kept, pool)
	}
	if len(kept) == len(c.pools) {
		return false
	}
	c.pools = kept
	c.save()
	_ = before
	return true
}

func (c *CPAConfig) SetImportJob(poolID string, job any) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for index, pool := range c.pools {
		if stringValue(pool["id"]) != poolID {
			continue
		}
		next := copyMap(pool)
		next["import_job"] = normalizeImportJob(job, false)
		c.pools[index] = next
		c.save()
		return copyMap(next)
	}
	return nil
}

func (c *CPAConfig) GetImportJob(poolID string) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, pool := range c.pools {
		if stringValue(pool["id"]) == poolID {
			return pool["import_job"]
		}
	}
	return nil
}

// CPARemoteFile 是远程文件列表的一项。
type CPARemoteFile struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// ListRemoteFiles 复刻 list_remote_files：
// GET {base_url}/v0/management/auth-files，Bearer secret_key 鉴权。
func ListRemoteFiles(ctx CtxLike, baseURL, secretKey string) ([]CPARemoteFile, error) {
	if baseURL == "" || secretKey == "" {
		return []CPARemoteFile{}, nil
	}
	target := trimRightSlash(baseURL) + "/v0/management/auth-files"
	body, status, err := httpGetJSON(ctx, target, map[string]string{
		"Authorization": "Bearer " + secretKey,
		"Accept":        "application/json",
	}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("remote list failed: HTTP %d", status)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, errors.New("remote list payload is invalid")
	}
	files, _ := payload["files"].([]any)
	if files == nil {
		return nil, errors.New("remote list payload is invalid")
	}

	items := make([]CPARemoteFile, 0, len(files))
	for _, item := range files {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := trimSpaceValue(entry["name"])
		if name == "" {
			continue
		}
		email := trimSpaceValue(entry["email"])
		if email == "" {
			email = trimSpaceValue(entry["account"])
		}
		items = append(items, CPARemoteFile{Name: name, Email: email})
	}
	return items, nil
}

// FetchRemoteAccessToken 复刻 fetch_remote_access_token。
func FetchRemoteAccessToken(ctx CtxLike, baseURL, secretKey, fileName string) (string, error) {
	if baseURL == "" || secretKey == "" || fileName == "" {
		return "", errors.New("invalid request")
	}
	target := trimRightSlash(baseURL) + "/v0/management/auth-files/download?name=" + urlEncode(fileName)
	body, status, err := httpGetJSON(ctx, target, map[string]string{
		"Authorization": "Bearer " + secretKey,
		"Accept":        "application/json",
	}, 30*time.Second)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("HTTP %d", status)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", errors.New("invalid payload")
	}
	token := trimSpaceValue(payload["access_token"])
	if token == "" {
		return "", errors.New("missing access_token")
	}
	return token, nil
}

// SanitizeCPAPool 复刻 sanitize_cpa_pool：剔除 secret_key 再对外。
func SanitizeCPAPool(pool map[string]any) map[string]any {
	if pool == nil {
		return nil
	}
	out := make(map[string]any, len(pool))
	for key, value := range pool {
		if key == "secret_key" {
			continue
		}
		out[key] = value
	}
	return out
}

// SanitizeCPAPools 批量清洗。
func SanitizeCPAPools(pools []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(pools))
	for _, pool := range pools {
		if sanitized := SanitizeCPAPool(pool); sanitized != nil {
			out = append(out, sanitized)
		}
	}
	return out
}

func newShortID() string {
	return randomHex(6)
}

func nowISOValue() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func trimSpaceValue(value any) string {
	return trimSpaceString(stringValue(value))
}
