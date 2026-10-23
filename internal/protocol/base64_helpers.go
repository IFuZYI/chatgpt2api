package protocol

import "encoding/base64"

// decodeBase64String 解 base64（容忍缺失的 padding）。
func decodeBase64String(value string) ([]byte, error) {
	trimmed := value
	if pad := len(trimmed) % 4; pad != 0 {
		trimmed += string(make([]byte, 4-pad))
		for i := len(value); i < len(trimmed); i++ {
			trimmed = trimmed[:i] + "=" + trimmed[i+1:]
		}
	}
	return base64.StdEncoding.DecodeString(trimmed)
}
