package api

import (
	"encoding/json"
	"net/url"
	"os"
)

// readFileBytes 读文件，不存在时返回错误。
func readFileBytes(path string) ([]byte, error) { return os.ReadFile(path) }

// unmarshalObject 解析 JSON 对象。
func unmarshalObject(payload []byte) (map[string]any, error) {
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// urlQueryEscape 是 url.QueryEscape 的短别名。
func urlQueryEscape(value string) string { return url.QueryEscape(value) }
