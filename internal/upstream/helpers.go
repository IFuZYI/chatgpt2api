package upstream

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strconv"

	_ "golang.org/x/image/webp"
)

// stringOfValue 是上游 payload 取值用的容错转换。
func stringOfValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case nil:
		return ""
	default:
		return ""
	}
}

// imageDimensions 读取图片宽高，失败返回 0,0,false。
func imageDimensions(payload []byte) (int, int, bool) {
	if len(payload) == 0 {
		return 0, 0, false
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(payload))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return 0, 0, false
	}
	return config.Width, config.Height, true
}

// numberValue 容错取数字（用于 update_time 这类可能是字符串的字段）。
func numberValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case string:
		var parsed float64
		if _, err := fmt.Sscanf(typed, "%f", &parsed); err == nil {
			return parsed
		}
		return 0
	default:
		return 0
	}
}
