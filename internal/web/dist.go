package web

import (
	"embed"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

//go:embed all:web_dist
var embedded embed.FS

// Dist 是前端静态资源的读取入口。
//
// 两种来源：
//   - 编译期：//go:embed all:web_dist —— 生产只发一个二进制。
//   - 运行期：CHATGPT2API_WEB_DIST 指向目录 —— 开发时改前端不用重编译 Go。
//
// 空目录无法 embed，所以 web_dist/.gitkeep 是必需的占位文件；未构建前端时
// 只有 .gitkeep，此时所有路径都查不到，fallback 也返回 nil（与 Python 的
// resolve_web_asset 在 web_dist 不存在时返回 None 行为一致）。
type Dist struct {
	dir string // 非空表示走运行期目录
}

// NewDist 根据环境变量选择来源。dir 为空则用编译期 embed。
func NewDist() *Dist {
	if dir := strings.TrimSpace(os.Getenv("CHATGPT2API_WEB_DIST")); dir != "" {
		return &Dist{dir: dir}
	}
	return &Dist{}
}

// Available 报告是否真的能提供文件（用于 /health 之类的自检）。
func (d *Dist) Available() bool {
	if d.dir != "" {
		info, err := os.Stat(filepath.Join(d.dir, "index.html"))
		return err == nil && !info.IsDir()
	}
	_, err := fs.Stat(embedded, "web_dist/index.html")
	return err == nil
}

// Resolve 复刻 Python 的 resolve_web_asset：
//
//	请求路径 -> index.html / <path> / <path>/index.html / <path>.html
//
// 只返回落在根目录内的普通文件，越界路径一律忽略。
func (d *Dist) Resolve(requestedPath string) ([]byte, string, bool) {
	clean := strings.Trim(strings.ReplaceAll(requestedPath, "\\", "/"), "/")
	var candidates []string
	if clean == "" {
		candidates = []string{"index.html"}
	} else {
		candidates = []string{
			clean,
			path.Join(clean, "index.html"),
			clean + ".html",
		}
	}
	for _, candidate := range candidates {
		if !safeRelative(candidate) {
			continue
		}
		if payload, ok := d.read(candidate); ok {
			return payload, candidate, true
		}
	}
	return nil, "", false
}

// Fallback 返回 SPA 的 index.html，供未知前端路由使用。
func (d *Dist) Fallback() ([]byte, bool) {
	return d.read("index.html")
}

// IsNextAsset 复刻 Python 对 `_next/` 前缀的特判：这类路径缺文件就是 404，
// 绝不 fallback 到 index.html，否则 Next.js 会拿到 HTML 当 JS 解析。
func IsNextAsset(requestedPath string) bool {
	clean := strings.Trim(strings.ReplaceAll(requestedPath, "\\", "/"), "/")
	return strings.HasPrefix(clean, "_next/")
}

func (d *Dist) read(relative string) ([]byte, bool) {
	if d.dir != "" {
		full := filepath.Join(d.dir, filepath.FromSlash(relative))
		root, err := filepath.Abs(d.dir)
		if err != nil {
			return nil, false
		}
		abs, err := filepath.Abs(full)
		if err != nil || !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
			return nil, false
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			return nil, false
		}
		payload, err := os.ReadFile(abs)
		if err != nil {
			return nil, false
		}
		return payload, true
	}
	payload, err := fs.ReadFile(embedded, path.Join("web_dist", relative))
	if err != nil {
		return nil, false
	}
	return payload, true
}

// safeRelative 拒绝绝对路径与上跳路径。
func safeRelative(relative string) bool {
	if relative == "" || strings.HasPrefix(relative, "/") {
		return false
	}
	for _, part := range strings.Split(relative, "/") {
		if part == ".." || part == "." || part == "" {
			return false
		}
	}
	return true
}
