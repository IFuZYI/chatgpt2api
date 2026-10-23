package web

import (
	"strings"
	"testing"
)

// embed 必须能读到编译进去的文件。
//
// 两种状态都合法：
//   - 前端已构建并拷入 web_dist：index.html 存在，Available()=true
//   - 只有 .gitkeep 占位（未构建前端）：Available()=false，但 .gitkeep 可读
//
// 这个测试只断言「embed 机制本身有效」，不依赖某一种状态。
func TestEmbeddedDistIsReadable(t *testing.T) {
	t.Setenv("CHATGPT2API_WEB_DIST", "")
	dist := NewDist()

	if dist.Available() {
		// 有真实前端：index.html 必须可读且是 HTML
		payload, resolved, ok := dist.Resolve("index.html")
		if !ok {
			t.Fatal("index.html advertised but not readable; go:embed is broken")
		}
		if resolved != "index.html" {
			t.Errorf("resolved = %q", resolved)
		}
		if !strings.Contains(strings.ToLower(string(payload)), "<html") &&
			!strings.Contains(strings.ToLower(string(payload)), "<!doctype") {
			t.Errorf("index.html does not look like HTML: %q", string(payload[:minInt(80, len(payload))]))
		}
		t.Logf("embedded frontend present (%d bytes)", len(payload))
		return
	}

	// 未构建前端：只剩占位文件，但 embed 仍必须可读
	payload, resolved, ok := dist.Resolve(".gitkeep")
	if !ok {
		t.Fatal("neither index.html nor .gitkeep readable; go:embed is broken")
	}
	if resolved != ".gitkeep" {
		t.Errorf("resolved = %q", resolved)
	}
	if len(payload) != 0 {
		t.Errorf(".gitkeep should be empty, got %d bytes", len(payload))
	}
	t.Log("no embedded frontend (placeholder only) — expected when web_dist is not built")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Resolve 的候选顺序：原路径 → <path>/index.html → <path>.html
func TestResolveCandidates(t *testing.T) {
	dist := NewDist()
	// 不存在的路径返回 false
	if _, _, ok := dist.Resolve("nope/missing.js"); ok {
		t.Error("missing path should not resolve")
	}
	// 越界路径必须拒绝
	if _, _, ok := dist.Resolve("../config.json"); ok {
		t.Error("path traversal must be rejected")
	}
	if _, _, ok := dist.Resolve("/etc/passwd"); ok {
		t.Error("absolute path must be rejected")
	}
}

func TestIsNextAsset(t *testing.T) {
	positives := []string{"_next/static/x.js", "/_next/static/x.js", "_next/data/a.json"}
	for _, path := range positives {
		if !IsNextAsset(path) {
			t.Errorf("%q should be a next asset", path)
		}
	}
	negatives := []string{"index.html", "accounts", "_nex/static/x.js"}
	for _, path := range negatives {
		if IsNextAsset(path) {
			t.Errorf("%q should not be a next asset", path)
		}
	}
}

// runtime 目录模式（CHATGPT2API_WEB_DIST）要能覆盖 embed。
func TestRuntimeDirOverridesEmbed(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir+"/index.html", "<html>runtime</html>"); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("CHATGPT2API_WEB_DIST", dir)
	dist := NewDist()
	if !dist.Available() {
		t.Fatal("runtime dir with index.html should be available")
	}
	payload, ok := dist.Fallback()
	if !ok {
		t.Fatal("fallback should read index.html")
	}
	if !strings.Contains(string(payload), "runtime") {
		t.Errorf("payload = %q", payload)
	}
}

// Resolve 返回的第二个值是「解析出的实际文件名」，MIME 必须按它推断。
// 按请求路径推断会让 `/` 与 `/accounts` 得到 application/octet-stream（实测踩到）。
func TestResolveReturnsActualFilename(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir+"/index.html", "<html>root</html>"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writeFile(dir+"/about.html", "<html>about</html>"); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("CHATGPT2API_WEB_DIST", dir)
	dist := NewDist()

	cases := []struct {
		requested    string
		wantResolved string
	}{
		{"", "index.html"},
		{"index.html", "index.html"},
		{"about", "about.html"},
	}
	for _, tc := range cases {
		_, resolved, ok := dist.Resolve(tc.requested)
		if !ok {
			t.Errorf("Resolve(%q) failed", tc.requested)
			continue
		}
		if resolved != tc.wantResolved {
			t.Errorf("Resolve(%q) resolved = %q, want %q", tc.requested, resolved, tc.wantResolved)
		}
	}
}
