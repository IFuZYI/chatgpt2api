package api_test

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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

// collectMarkdown 枚举**被 git 跟踪**的 markdown。
//
// 为什么不用 filepath.Walk：走文件系统会把 gitignored 的本地文件也读进来，
// 于是 `go test ./...` 的结果取决于开发机的本地状态 —— 实测在
// .learnings/ 放一个提到假端点的文件，就能让 TestDocumentedEndpointsExist
// 在干净检出上失败。这与 AGENTS.md 规则 14（go 的 ./... 吃掉 node_modules）
// 是同一类 bug：测试输入必须是仓库内容，不是工作区内容。
//
// git ls-files 顺带把 .superpowers/、.learnings/、dogfood-output/、
// repro_outputs/ 全部排除掉 —— 前三个是本地 scratch，最后一个是历史快照
// （里面会刻意提到已删除的路径）。
func collectMarkdown(root string) (map[string]string, error) {
	out, err := gitLsFiles(root, "*.md")
	if err != nil {
		return nil, err
	}
	docs := make(map[string]string, len(out))
	for _, rel := range out {
		payload, readErr := os.ReadFile(filepath.Join(root, rel))
		if readErr != nil {
			continue
		}
		docs[rel] = string(payload)
	}
	return docs, nil
}

// gitLsFiles 返回仓库里匹配 pattern 的已跟踪文件（相对仓库根）。
func gitLsFiles(root, pattern string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", pattern)
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var out []string
	for _, line := range strings.Split(string(output), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out, nil
}

// relPathForTest 只保留仓库内相对路径，让失败信息简短、也便于按名字判断
// （例如「这份是不是 README」）。
//
// walk 的起点是 `../..`，所以原始路径形如 `../../docs/deployment.md`；
// 这里剥掉前导的 `../`，得到 `docs/deployment.md`。
func relPathForTest(path string) string {
	slashed := filepath.ToSlash(path)
	for strings.HasPrefix(slashed, "../") {
		slashed = strings.TrimPrefix(slashed, "../")
	}
	// 若工作目录已在仓库内（路径里带仓库名），再从仓库名之后截取。
	if idx := strings.Index(slashed, "chatgpt2api/"); idx >= 0 {
		return slashed[idx+len("chatgpt2api/"):]
	}
	return slashed
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

// README 声明的 ADR 数量必须与实际文件数一致。
//
// 与路由数量同一类：README 写着「N 条架构决策记录」，加一篇 ADR 不会让
// 任何测试失败，只有读文档的人会发现数字不对（本次审计就是这样发现的：
// 加了 ADR-0014 之后两版 README 仍写 13）。
//
// 改 ADR 数量时若这条失败，请同步更新 README.md 与 README.en.md 的
// 「相关文档 / Documentation」表。
func TestADRCountMatchesDocumentedNumber(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	adrDir := filepath.Join(repoRoot, "docs", "adr")

	entries, err := os.ReadDir(adrDir)
	if err != nil {
		t.Fatalf("读 ADR 目录失败: %v", err)
	}
	actual := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		// 只数编号文件（0001-…），跳过 README/index 之类。
		if len(e.Name()) >= 4 && e.Name()[0] >= '0' && e.Name()[0] <= '9' {
			actual++
		}
	}
	if actual == 0 {
		t.Fatal("没数到任何 ADR —— 目录结构可能变了")
	}

	docs, err := collectMarkdown(repoRoot)
	if err != nil {
		t.Fatalf("收集文档失败: %v", err)
	}

	// 两版 README 都必须声明同一个数字。
	re := regexp.MustCompile(`(\d+)\s*(?:条架构决策记录|architecture decision records)`)
	checked := 0
	for path, body := range docs {
		if path != "README.md" && path != "README.en.md" {
			continue
		}
		m := re.FindStringSubmatch(body)
		if m == nil {
			continue
		}
		checked++
		if m[1] != strconv.Itoa(actual) {
			t.Errorf("%s 声明 %s 条 ADR，实际 %d 条 —— 请同步更新两版 README",
				path, m[1], actual)
		}
	}
	if checked != 2 {
		t.Errorf("只检查了 %d 份 README（应为 2）—— 文档结构可能变了", checked)
	}
}

// ADR-0009 的「测试面」表格数字必须与实际测试数一致。
//
// 为什么需要：这份表在 2.2.3 审计里刚被"修正"过，但改的当下就写错了
// （service 写成 269，实际 266；api 写 130，实际 131；合计 776，实际 774）——
// 靠人肉数文件必然出错，而且下一个提交就会让它再漂一次。
//
// 口径与 ADR 自己声明的一致：顶层 `func Test` 函数数（不是 go test -v 的
// 用例数，后者含子测试，约 1.4 倍）。
func TestADRTestCountsMatchReality(t *testing.T) {
	repoRoot := filepath.Join("..", "..")

	payload, err := os.ReadFile(filepath.Join(repoRoot, "docs", "adr", "0009-go-rewrite-scope-and-gaps.md"))
	if err != nil {
		t.Fatalf("读 ADR-0009: %v", err)
	}
	body := string(payload)

	// 只解析「重写时 / 2.2.3 审计后」那张表的数据行。
	rowRe := regexp.MustCompile(`(?m)^\| ` + "`internal/([a-z]+)`" + ` \| (\d+) \| (\d+) \|`)
	rows := rowRe.FindAllStringSubmatch(body, -1)
	if len(rows) == 0 {
		t.Fatal("ADR-0009 里没解析到测试面表格（格式可能变了）")
	}

	sumAudited := 0
	for _, m := range rows {
		pkg, audited := m[1], atoiOrZero(m[3])
		sumAudited += audited

		dir := filepath.Join(repoRoot, "internal", pkg)
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			t.Errorf("internal/%s 读不到: %v", pkg, readErr)
			continue
		}
		actual := 0
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			content, fileErr := os.ReadFile(filepath.Join(dir, e.Name()))
			if fileErr != nil {
				continue
			}
			actual += len(regexp.MustCompile(`(?m)^func Test[A-Za-z0-9_]+`).FindAllString(string(content), -1))
		}
		if actual != audited {
			t.Errorf("internal/%s: ADR 写 %d 个测试，实际 %d —— 请更新 ADR-0009 的表格", pkg, audited, actual)
		}
	}

	// 合计行也要对。
	totalRe := regexp.MustCompile(`\*\*合计\*\* \| \*\*\d+\*\* \| \*\*(\d+)\*\*`)
	if m := totalRe.FindStringSubmatch(body); m != nil {
		if docTotal := atoiOrZero(m[1]); docTotal != sumAudited {
			t.Errorf("ADR 合计写 %d，各行相加为 %d", docTotal, sumAudited)
		}
	}
}
