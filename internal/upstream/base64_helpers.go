package upstream

import (
	"encoding/base64"
	"strings"
)

// decodeBase64Image 解 base64 图片（容忍缺失 padding）。
func decodeBase64Image(value string) ([]byte, error) {
	trimmed := strings.TrimSpace(value)
	if pad := len(trimmed) % 4; pad != 0 {
		trimmed += strings.Repeat("=", 4-pad)
	}
	return base64.StdEncoding.DecodeString(trimmed)
}
