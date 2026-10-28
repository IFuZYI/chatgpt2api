package api_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// scripts/build-release.sh 与 release.yml 必须产出等价的构建命令。
//
// 为什么需要：Actions 被暂停时只能用本地脚本构建产物，而脚本是 release.yml
// 的手工复制品。两者一旦漂移，本地构建出的产物就与 CI 不一致 —— 而本地产物
// 正是拿去发布的那份。
//
// **为什么不用 strings.Contains**：这个测试最初是「文件里有没有出现
// -trimpath / -s -w / BuildVersion」—— 实测三种破坏都能骗过它：
//  1. 删掉 go build 行的 -trimpath（注释里仍出现该词）→ 通过
//  2. 删掉整个 -ldflags="$ldflags"                    → 通过
//  3. 删掉前端构建与暂存整段                            → 通过
//
// 那种断言守的是「文本里提到过」，不是「命令里用了」。现在改成**解析出
// 两侧实际的 go build 命令**再比 flag 集合，并断言脚本里存在前端暂存与
// index.html 断言（这正是本项目真实踩过的两个坑）。
func TestReleaseScriptMatchesWorkflow(t *testing.T) {
	repoRoot := filepath.Join("..", "..")

	workflow, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("读 release.yml: %v", err)
	}
	script, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "build-release.sh"))
	if err != nil {
		t.Fatalf("读 build-release.sh: %v", err)
	}
	wf, sh := string(workflow), string(script)

	// 1) 平台矩阵必须一致。
	wfTargets := parseWorkflowTargets(wf)
	shTargets := parseScriptTargets(sh)
	if len(wfTargets) == 0 || len(shTargets) == 0 {
		t.Fatalf("解析平台矩阵失败：release.yml=%v script=%v", wfTargets, shTargets)
	}
	if join(wfTargets) != join(shTargets) {
		t.Errorf("平台矩阵不一致\n  release.yml:   %v\n  build-release.sh: %v", wfTargets, shTargets)
	}

	// 2) 实际 go build 命令的 flag 集合必须一致（不再看注释里出现过什么）。
	wfFlags := parseGoBuildFlags(wf, "go build")
	shFlags := parseGoBuildFlags(sh, "go build")
	if len(wfFlags) == 0 || len(shFlags) == 0 {
		t.Fatalf("解析 go build flag 失败：release.yml=%v script=%v", wfFlags, shFlags)
	}
	if join(wfFlags) != join(shFlags) {
		t.Errorf("go build flag 不一致\n  release.yml:   %v\n  build-release.sh: %v", wfFlags, shFlags)
	}
	for _, must := range []string{"-trimpath", "-ldflags"} {
		if !contains(shFlags, must) {
			t.Errorf("build-release.sh 的 go build 缺少 %s（实际 flag: %v）", must, shFlags)
		}
		if !contains(wfFlags, must) {
			t.Errorf("release.yml 的 go build 缺少 %s（实际 flag: %v）", must, wfFlags)
		}
	}

	// 3) -ldflags 的值里必须真的含 -s -w 与 BuildVersion 注入。
	//    只检查 flag 名不够：`-ldflags=""` 也带 -ldflags。
	for name, body := range map[string]string{"release.yml": wf, "build-release.sh": sh} {
		ld := parseLdflagsValue(body)
		if ld == "" {
			t.Errorf("%s: 没解析到 -ldflags 的值", name)
			continue
		}
		for _, must := range []string{"-s", "-w", "internal/config.BuildVersion="} {
			if !strings.Contains(ld, must) {
				t.Errorf("%s: -ldflags 缺少 %s（实际值 %q）", name, must, ld)
			}
		}
	}

	// 4) 产物命名模板必须一致：脚本里必须按 version/os/arch 顺序拼。
	if !strings.Contains(sh, `chatgpt2api_${version}_${goos}_${goarch}`) {
		t.Error("build-release.sh 的产物命名不是 chatgpt2api_${version}_${goos}_${goarch}")
	}
	if !strings.Contains(wf, `"${BINARY}_${version}_${GOOS}_${GOARCH}"`) {
		t.Error("release.yml 的产物命名不是 ${BINARY}_${version}_${GOOS}_${GOARCH}")
	}

	// 5) 脚本必须构建并暂存前端，且有 index.html 断言。
	//    少了前端会产出访问 / 与 /accounts 全 404 的二进制（真实踩过）。
	for _, must := range []string{"npm run build", "cp -r web/out/. internal/web/web_dist/", "internal/web/web_dist/index.html"} {
		if !strings.Contains(sh, must) {
			t.Errorf("build-release.sh 缺少前端构建/暂存/断言的关键片段 %q", must)
		}
	}
	// 6) 两侧都必须保住被跟踪的 .gitkeep（删掉它会让产物被标记为 dirty）。
	for name, body := range map[string]string{"release.yml": wf, "build-release.sh": sh} {
		if !strings.Contains(body, ".gitkeep") {
			t.Errorf("%s: 没有保住 .gitkeep —— 暂存前端会删掉被跟踪文件，产物会被标记为 dirty", name)
		}
	}
}

// parseGoBuildFlags 从源码里抽出真正执行的那条 go build 命令的 flag 集合。
//
// 只看以 `go build` 开头（允许前置空白/环境变量）的行，跳过注释行 ——
// 这正是旧实现被注释里的 -trimpath 骗过的原因。
func parseGoBuildFlags(body, prefix string) []string {
	flagRe := regexp.MustCompile(`(-[a-zA-Z][a-zA-Z-]*)`)
	var out []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		idx := strings.Index(trimmed, prefix)
		if idx < 0 {
			continue
		}
		cmd := trimmed[idx:]
		// 截到输出参数之前（-o ... 之后的 ./cmd/server 不含 flag）
		for _, m := range flagRe.FindAllString(cmd, -1) {
			if strings.HasPrefix(m, "-X") || m == "-" {
				continue
			}
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return dedupeStrings(out)
}

// parseLdflagsValue 抽出 -ldflags 的赋值内容（两种写法都支持：
// -ldflags="$ldflags" 与 -ldflags="-s -w -X ..."）。
func parseLdflagsValue(body string) string {
	// 先找直接赋值：-ldflags="..."
	re := regexp.MustCompile(`-ldflags="([^"]*)"`)
	var found []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(trimmed, -1) {
			found = append(found, m[1])
		}
	}
	// 间接引用（-ldflags="$ldflags"）时，回到变量定义处取值。
	var out []string
	for _, v := range found {
		if strings.HasPrefix(v, "$") {
			// 支持 $name 与 ${name} 两种引用形式。
			varName := strings.TrimPrefix(v, "$")
			varName = strings.TrimPrefix(varName, "{")
			varName = strings.TrimSuffix(varName, "}")
			// 变量定义可能是 `ldflags="..."`（行首）或 `  ldflags="..."`（缩进），
			// 也可能带 export。用宽松的 \b 边界。
			varRe := regexp.MustCompile(`(?m)^\s*(?:export\s+)?` + regexp.QuoteMeta(varName) + `="([^"]*)"`)
			if m := varRe.FindStringSubmatch(body); m != nil {
				out = append(out, m[1])
			}
			continue
		}
		out = append(out, v)
	}
	return strings.Join(out, " ")
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func join(list []string) string { return strings.Join(list, ",") }

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// parseWorkflowTargets 从 release.yml 的 matrix.include 里取 goos/goarch/goarm。
func parseWorkflowTargets(body string) []string {
	re := regexp.MustCompile(`- goos:\s*(\S+)\s*\n\s*goarch:\s*(\S+)(?:\s*\n\s*goarm:\s*"?(\d+)"?)?`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1]+"/"+m[2]+"/"+m[3])
	}
	return out
}

// parseScriptTargets 从 build-release.sh 的 targets 数组里取三元组
// （格式：$'goos<TAB>goarch<TAB>goarm'）。
func parseScriptTargets(body string) []string {
	re := regexp.MustCompile(`\$\'([a-z0-9]+)\\t([a-z0-9]+)\\t([0-9]*)\'`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1]+"/"+m[2]+"/"+m[3])
	}
	return out
}
