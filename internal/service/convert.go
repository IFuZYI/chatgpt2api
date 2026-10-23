package service

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ---- 通用类型转换。全部容错，绝不 panic：数据来自用户文件。 ----

func stringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case float64:
		// JSON 数字统一走 float64；整数值去掉小数点。
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func intValue(value any) int {
	switch typed := value.(type) {
	case nil:
		return 0
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0
		}
		if parsed, err := strconv.Atoi(trimmed); err == nil {
			return parsed
		}
		if parsed, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return int(parsed)
		}
		return 0
	case bool:
		if typed {
			return 1
		}
		return 0
	default:
		return 0
	}
}

func floatValue(value any) float64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case float64:
		return typed
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
			return parsed
		}
		return 0
	default:
		return 0
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func readFileString(path string) (string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func writeFileString(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// itoa 是 strconv.Itoa 的短别名，日志文案里拼数字用。
func itoa(value int) string { return strconv.Itoa(value) }

// fileExists 报告路径是否为普通文件。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
