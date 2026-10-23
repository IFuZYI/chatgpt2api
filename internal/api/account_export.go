package api

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// errNotConfigured 表示某项可选依赖未注入。
var errNotConfigured = errors.New("dependency not configured")

// buildAccountZip 把账号导出成 zip（每个账号一个 json 文件）。
func buildAccountZip(items []map[string]any) ([]byte, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	usedNames := map[string]bool{}

	for index, item := range items {
		base := exportFileName(item, index+1)
		name := base + ".json"
		counter := 2
		for usedNames[name] {
			name = fmt.Sprintf("%s_%d.json", base, counter)
			counter++
		}
		usedNames[name] = true

		entry, err := writer.Create(name)
		if err != nil {
			continue
		}
		encoded, err := marshalJSON(item)
		if err != nil {
			continue
		}
		if _, err := entry.Write([]byte(encoded + "\n")); err != nil {
			continue
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// exportFileName 用邮箱或 token 前缀做文件名（避免路径穿越）。
func exportFileName(item map[string]any, index int) string {
	candidate := strings.TrimSpace(stringValueOf(item["email"]))
	if candidate == "" {
		candidate = strings.TrimSpace(stringValueOf(item["access_token"]))
		if len(candidate) > 12 {
			candidate = candidate[:12]
		}
	}
	if candidate == "" {
		candidate = fmt.Sprintf("account-%d", index)
	}
	// 只保留安全字符，防路径穿越。
	var builder strings.Builder
	for _, ch := range candidate {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '@', ch == '.':
			builder.WriteRune(ch)
		default:
			builder.WriteRune('_')
		}
	}
	name := strings.Trim(builder.String(), "._")
	if name == "" {
		return fmt.Sprintf("account-%d", index)
	}
	return name
}
