package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 文档里写的端点必须真实存在。
//
// 为什么需要：本轮审计在 README/feature-status 里发现「77 条路由」这样的
// 陈述性漂移（已加 TestRouteCountMatchesDocumentedNumber 守住数量）。
// 这条守的是**路径级**漂移 —— 文档写 `POST /v1/foo` 但代码里根本没有，
// 照着文档写客户端的人会拿到 404，而没有任何测试会失败。
//
// 只检查本服务自己的命名空间（`/api/*` 与 `/v1/*`）。上游路径
// （`/backend-api/*`）故意排除：那是 OpenAI 的接口，不是我们注册的。
//
// 排除项按需收敛：正则会把 `POST /v1/ppt|psd/generations` 这种「一行写两个
// 端点」的写法拆开分别校验，所以不需要为它开特例。
func TestDocumentedEndpointsExist(t *testing.T) {
	deps := newTestDeps(t)
	router := newTestRouter(t, deps)

	// 真实注册的路由，参数段规范成 ":X" 便于比对。
	registered := map[string]bool{}
	for _, r := range router.Routes() {
		if r.Method == http.MethodHead || r.Method == http.MethodOptions {
			continue
		}
		registered[normalizeDocPath(r.Path)] = true
	}

	// 文档根目录：测试的工作目录是 internal/api，仓库根在上两级。
	repoRoot := filepath.Join("..", "..")
	docs, err := collectMarkdown(repoRoot)
	if err != nil {
		t.Fatalf("收集文档失败: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("没找到任何 markdown —— 路径假设可能不对")
	}

	// 匹配 “GET /api/foo” / “POST ` /v1/bar” 这类写法。
	re := regexp.MustCompile(`(?:GET|POST|PUT|PATCH|DELETE)\s+` + "`?" + `((?:/api|/v1)/[a-zA-Z0-9/_:{}.*|-]*)`)

	var missing []string
	for path, body := range docs {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			for _, candidate := range splitAlternatives(m[1]) {
				norm := normalizeDocPath(candidate)
				if norm == "" || norm == "/api" || norm == "/v1" {
					continue
				}
				if registered[norm] {
					continue
				}
				missing = append(missing, candidate+"   ← "+path)
			}
		}
	}

	if len(missing) > 0 {
		t.Errorf("文档提到但代码里没注册的端点（%d 个）:\n  %s",
			len(missing), strings.Join(dedupe(missing), "\n  "))
	}
}

// splitAlternatives 把 `/v1/ppt|psd/generations` 拆成两个完整路径。
//
// 文档里常用这种简写表示「同一形态的两个端点」。拆解规则：
// 前缀取 `|` 之前最后一个 `/` 之前的部分，后缀取 `|` 之后第一个 `/` 起
// 的部分，两侧分别与前缀、后缀拼接。例如
//
//	/v1/ppt|psd/generations  →  /v1/ppt/generations, /v1/psd/generations
func splitAlternatives(path string) []string {
	i := strings.Index(path, "|")
	if i < 0 {
		return []string{path}
	}
	slash := strings.LastIndex(path[:i], "/")
	if slash < 0 {
		return []string{path}
	}
	prefix := path[:slash+1]  // "/v1/"
	left := path[slash+1 : i] // "ppt"
	rest := path[i+1:]        // "psd/generations"
	right, suffix := rest, ""
	if j := strings.Index(rest, "/"); j >= 0 {
		right, suffix = rest[:j], rest[j:]
	}
	return []string{prefix + left + suffix, prefix + right + suffix}
}

// normalizeDocPath 把 gin 的 `:id` / `*rest` 与文档里的 `:id`、`{id}` 统一成 `:X`。
func normalizeDocPath(p string) string {
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return ""
	}
	repl := regexp.MustCompile(`[:*][a-zA-Z_][a-zA-Z0-9_]*`)
	p = repl.ReplaceAllString(p, ":X")
	p = regexp.MustCompile(`\{[a-zA-Z_][a-zA-Z0-9_]*\}`).ReplaceAllString(p, ":X")
	return p
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// collectMarkdown 读仓库根下所有 markdown。
//
// 跳过 node_modules/.git/构建产物，以及 `.superpowers/`（内部规划与验证
// 报告目录）—— 那些文件会刻意提到「Python 有但本仓库已删除」的路径
// （例如 `/api/third-party-apps`），属于历史分析记录，不是用户文档。
func collectMarkdown(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 忽略无权限的目录
		}
		if info.IsDir() {
			base := filepath.Base(path)
			switch base {
			case "node_modules", ".git", "out", ".next", "vendor", "dist", ".superpowers":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		payload, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		out[relPathForTest(path)] = string(payload)
		return nil
	})
	return out, err
}

// relPathForTest 只保留仓库内相对路径，让失败信息简短。
func relPathForTest(path string) string {
	idx := strings.Index(path, "chatgpt2api/")
	if idx >= 0 {
		return path[idx+len("chatgpt2api/"):]
	}
	return filepath.ToSlash(path)
}

// 文档里点名的测试文件必须存在。
//
// 为什么需要：docs/account-lifecycle.md 的「测试清单」曾引用
// `retry_policy_test.go` —— 那个文件在某次重构里被合并进
// `audit_regression_test.go`，文档没跟着改。读者按图索骥会找不到文件，
// 而没有任何测试会失败。
//
// 只检查 `*_test.go`：这类引用是「去哪里看回归覆盖」的指路牌，
// 指错了等于把读者引到荒地。Python 文件名不在检查范围内 ——
// 那些是已归档到另一分支的历史实现，在 archive/ADR 里被有意提及。
func TestDocumentedTestFilesExist(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	docs, err := collectMarkdown(repoRoot)
	if err != nil {
		t.Fatalf("收集文档失败: %v", err)
	}

	// 仓库里真实存在的 *go 文件，按 basename 索引。
	existing := map[string]bool{}
	err = filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			existing[info.Name()] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 Go 文件失败: %v", err)
	}
	if len(existing) == 0 {
		t.Fatal("没找到任何 .go 文件 —— 路径假设可能不对")
	}

	re := regexp.MustCompile("`([a-zA-Z0-9_]+_test\\.go)`")
	var missing []string
	for path, body := range docs {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			if !existing[m[1]] {
				missing = append(missing, m[1]+"   ← "+path)
			}
		}
	}
	if len(missing) > 0 {
		t.Errorf("文档点名但仓库里不存在的测试文件（%d 个）:\n  %s",
			len(missing), strings.Join(dedupe(missing), "\n  "))
	}
}
