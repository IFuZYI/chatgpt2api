package protocol

import "encoding/json"

// decodeObject 宽松解码：失败返回 false（对应 Python 的 json.JSONDecodeError 分支）。
func decodeObject(payload string, target *map[string]any) bool {
	if err := json.Unmarshal([]byte(payload), target); err != nil {
		return false
	}
	return *target != nil
}
