package upstream

import "strconv"

// strconvFormatInt / strconvFormatFloat 隔离对 strconv 的直接依赖，
// 让 turnstile.go 的 Python 语义映射更直观。
func strconvFormatInt(value int64) string { return strconv.FormatInt(value, 10) }

func strconvFormatFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
