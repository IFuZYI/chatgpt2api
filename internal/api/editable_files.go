package api

import (
	"path/filepath"
	"strings"
)

// resolveEditableFile 把 /files/{path} 解析到 data/files 下的真实文件。
//
// 复刻 services/editable_file_task_service.py:159-165 的 public_file_path：
// 越界（含符号链接越界）与不存在的文件都返回错误，由调用方转 404。
//
// 实现委托给任务服务，保证「落盘目录」与「读取目录」是同一个根：
// 两处各自拼 root 的话，配置的 data 目录一变就会出现「任务写 A、下载读 B」。
func (d *Deps) resolveEditableFile(relative string) (string, error) {
	if d.EditableTasks == nil {
		return "", errNotConfigured
	}
	clean := strings.TrimLeft(strings.ReplaceAll(relative, "\\", "/"), "/")
	if clean == "" {
		return "", errNotConfigured
	}
	return d.EditableTasks.PublicFilePath(filepath.ToSlash(clean))
}
