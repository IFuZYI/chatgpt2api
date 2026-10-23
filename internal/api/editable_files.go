package api

import (
	"path/filepath"
	"strings"
)

// resolveEditableFile 把 /files/{path} 解析到 data/files 下的真实文件。
//
// 拒绝越界路径（与 Python 的 public_file_path 一致：relative_to 校验）。
func (d *Deps) resolveEditableFile(relative string) (string, error) {
	clean := strings.TrimLeft(strings.ReplaceAll(relative, "\\", "/"), "/")
	if clean == "" {
		return "", errNotConfigured
	}
	root, err := filepath.Abs(d.Config.FilesDir())
	if err != nil {
		return "", err
	}
	full, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(clean)))
	if err != nil {
		return "", err
	}
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", errNotConfigured
	}

	// 解析符号链接后再校验（同 localImagePath 的理由）：
	// 不解析的话，data/files/ 下指向 config.json 的软链会被当成合法文件读出去。
	resolvedRoot, rootErr := filepath.EvalSymlinks(root)
	resolvedFull, fullErr := filepath.EvalSymlinks(full)
	if rootErr == nil && fullErr == nil {
		if resolvedFull != resolvedRoot &&
			!strings.HasPrefix(resolvedFull, resolvedRoot+string(filepath.Separator)) {
			return "", errNotConfigured
		}
	}

	info, err := statFile(full)
	if err != nil || info.IsDir() {
		return "", errNotConfigured
	}
	return full, nil
}
