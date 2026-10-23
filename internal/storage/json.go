package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// JSONBackend 复刻 services/storage/json_storage.py。
// 关键行为：任何读取失败（含 JSON 语法错误）都返回空列表，不中断启动。
type JSONBackend struct {
	accountsPath string
	authKeysPath string
}

func NewJSONBackend(accountsPath, authKeysPath string) (*JSONBackend, error) {
	if err := os.MkdirAll(filepath.Dir(accountsPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(authKeysPath), 0o755); err != nil {
		return nil, err
	}
	return &JSONBackend{accountsPath: accountsPath, authKeysPath: authKeysPath}, nil
}

// loadJSONList 读 JSON 数组，逐条容错。
//
// 关键：必须逐元素解析，不能直接 Unmarshal 到 []map[string]any。
// 后者只要数组里有一个非对象元素（null / 字符串 / 数字）就整体失败返回空，
// 于是整个号池消失 —— 而调用方紧接着 saveAccounts 会把空池写回磁盘，
// 全池 token 永久丢失。Python json.loads 后逐条 isinstance(dict) 判断，
// 只跳过坏条目。
func loadJSONList(path string) []map[string]any {
	payload, err := os.ReadFile(path)
	if err != nil {
		return []map[string]any{}
	}
	var raw []any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return []map[string]any{}
	}
	items := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		if item, ok := entry.(map[string]any); ok && item != nil {
			items = append(items, item)
		}
	}
	return items
}

// saveJSONValue 原子写：先写同目录临时文件 + fsync，再 rename 覆盖。
//
// 为什么不能用 os.WriteFile：进程在写入中途退出（SIGKILL、容器 stop 超时）
// 会留下截断的 JSON，下次启动读不出来，等于整份账号全丢。
// rename 在同一文件系统上是原子的 —— 读者要么看到旧文件、要么看到新文件。
func saveJSONValue(path string, value any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}

	if _, err := tmp.Write(payload); err != nil {
		cleanup()
		return err
	}
	// fsync 数据：没有它，rename 之后断电仍可能拿到空文件。
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func (b *JSONBackend) LoadAccounts() ([]map[string]any, error) {
	return loadJSONList(b.accountsPath), nil
}

func (b *JSONBackend) SaveAccounts(accounts []map[string]any) error {
	return saveJSONValue(b.accountsPath, accounts)
}

func (b *JSONBackend) LoadAuthKeys() ([]map[string]any, error) {
	payload, err := os.ReadFile(b.authKeysPath)
	if err != nil {
		return []map[string]any{}, nil
	}
	// 兼容 {"items": [...]} 与裸数组两种历史格式。
	var wrapper struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(payload, &wrapper); err == nil && wrapper.Items != nil {
		return wrapper.Items, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(payload, &items); err != nil {
		return []map[string]any{}, nil
	}
	return items, nil
}

func (b *JSONBackend) SaveAuthKeys(authKeys []map[string]any) error {
	return saveJSONValue(b.authKeysPath, map[string]any{"items": authKeys})
}

func (b *JSONBackend) HealthCheck() map[string]any {
	accountsExists := fileExists(b.accountsPath)
	authKeysExists := fileExists(b.authKeysPath)
	if accountsExists {
		if _, err := os.ReadFile(b.accountsPath); err != nil {
			return map[string]any{"status": "unhealthy", "backend": "json", "error": err.Error()}
		}
	}
	return map[string]any{
		"status":                "healthy",
		"backend":               "json",
		"file_exists":           accountsExists,
		"file_path":             b.accountsPath,
		"auth_keys_file_exists": authKeysExists,
		"auth_keys_file_path":   b.authKeysPath,
	}
}

func (b *JSONBackend) BackendInfo() map[string]any {
	return map[string]any{
		"type":                  "json",
		"description":           "本地 JSON 文件存储",
		"file_path":             b.accountsPath,
		"file_exists":           fileExists(b.accountsPath),
		"auth_keys_file_path":   b.authKeysPath,
		"auth_keys_file_exists": fileExists(b.authKeysPath),
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// maskPassword 复刻 Python 的 _mask_password：只保留用户名，密码打码。
func maskPassword(url string) string {
	scheme, rest, ok := splitScheme(url)
	if !ok {
		return url
	}
	at := indexByte(rest, '@')
	if at < 0 {
		return url
	}
	credentials, host := rest[:at], rest[at+1:]
	colon := indexByte(credentials, ':')
	if colon < 0 {
		return url
	}
	return fmt.Sprintf("%s://%s:****@%s", scheme, credentials[:colon], host)
}

func splitScheme(url string) (string, string, bool) {
	for i := 0; i+2 < len(url); i++ {
		if url[i] == ':' && url[i+1] == '/' && url[i+2] == '/' {
			return url[:i], url[i+3:], true
		}
	}
	return "", url, false
}

func indexByte(text string, ch byte) int {
	for i := 0; i < len(text); i++ {
		if text[i] == ch {
			return i
		}
	}
	return -1
}
