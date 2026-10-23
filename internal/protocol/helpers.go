package protocol

import (
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strconv"

	_ "golang.org/x/image/webp"
)

// imageSizeFromBytes 读取图片宽高，失败返回 ok=false。
func imageSizeFromBytes(payload []byte) (int, int, bool) {
	if len(payload) == 0 {
		return 0, 0, false
	}
	config, _, err := image.DecodeConfig(bytesReader(payload))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return 0, 0, false
	}
	return config.Width, config.Height, true
}

func intValueOf(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		if parsed, err := strconv.Atoi(typed); err == nil {
			return parsed
		}
		return 0
	default:
		return 0
	}
}

func stringOf(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
