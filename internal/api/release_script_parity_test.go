package api_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// scripts/build-release.sh 的目标矩阵必须与 release.yml 保持一致。
//
// 为什么需要：Actions 被暂停时只能用本地脚本构建产物，而这个脚本是
// release.yml 的**手工复制品**。两者一旦漂移（有人给 CI 加了新平台、
// 或改了 ldflags，却没同步脚本），本地构建出的产物就与 CI 的不一致 ——
// 而本地产物正是拿去发布的那份。
//
// 测试比对的是「平台三元组集合」与「是否含版本注入」这两件最要紧的事，
// 不去逐字比对 YAML 与 shell（那太脆）。
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

	wfTargets := parseWorkflowTargets(string(workflow))
	shTargets := parseScriptTargets(string(script))

	if len(wfTargets) == 0 {
		t.Fatal("从 release.yml 没解析出任何目标 —— 解析逻辑可能过时了")
	}
	if len(shTargets) == 0 {
		t.Fatal("从 build-release.sh 没解析出任何目标 —— 解析逻辑可能过时了")
	}

	wfSorted := append([]string(nil), wfTargets...)
	shSorted := append([]string(nil), shTargets...)
	sort.Strings(wfSorted)
	sort.Strings(shSorted)

	if strings.Join(wfSorted, ",") != strings.Join(shSorted, ",") {
		t.Errorf("平台矩阵不一致\n  release.yml:   %v\n  build-release.sh: %v",
			wfSorted, shSorted)
	}

	// 两边都必须注入版本号 —— 漏掉它产出的二进制会显示 0.0.0。
	if !strings.Contains(string(workflow), "internal/config.BuildVersion=") {
		t.Error("release.yml 缺少 BuildVersion 注入")
	}
	if !strings.Contains(string(script), "internal/config.BuildVersion=") {
		t.Error("build-release.sh 缺少 BuildVersion 注入")
	}
	// 两边都必须去符号表、去构建机路径。
	for name, body := range map[string]string{
		"release.yml":      string(workflow),
		"build-release.sh": string(script),
	} {
		if !strings.Contains(body, "-trimpath") {
			t.Errorf("%s 缺少 -trimpath", name)
		}
		if !strings.Contains(body, "-s -w") {
			t.Errorf("%s 缺少 -s -w", name)
		}
	}
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
