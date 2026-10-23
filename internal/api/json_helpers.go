package api

import "encoding/json"

// marshalJSON 序列化 chunk，关闭 HTML 转义（与 Python 的 ensure_ascii=False 对齐）。
func marshalJSON(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}
