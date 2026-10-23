package protocol

import (
	"math"
	"strconv"
	"strings"
)

// itoa 是整数转字符串的短别名。
func itoa(value int) string { return strconv.Itoa(value) }

// formatFloat 复刻 Python 的 str(float)：整数值也带 ".0"。
func formatFloat(value float64) string {
	if value == math.Trunc(value) {
		return strconv.FormatInt(int64(value), 10) + ".0"
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// orDefaultValue 空字符串时返回 fallback。
func orDefaultValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
