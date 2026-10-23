package web

import "os"

// writeFile 是测试用的写文件助手。
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
