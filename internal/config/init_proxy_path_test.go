package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// --init-proxy-config 的路径解析必须与主程序的 base-dir 一致。
//
// 回归点：早期实现只认 CHATGPT2API_CONFIG_FILE，否则硬编码 /app/config.json
// —— 设了 CHATGPT2API_BASE_DIR 也不生效（与主程序行为不一致），
// 变量存在但为空时得到空路径并报
// `write config : open : no such file or directory`（完全不指向该设哪个变量）。
func TestInitProxyRuntimeConfigPathPrecedence(t *testing.T) {
	baseDir := t.TempDir()

	t.Run("CHATGPT2API_CONFIG_FILE 优先", func(t *testing.T) {
		t.Setenv("CHATGPT2API_CONFIG_FILE", "/explicit/config.json")
		t.Setenv("CHATGPT2API_BASE_DIR", baseDir)
		if got := initProxyRuntimeConfigPath(); got != "/explicit/config.json" {
			t.Errorf("got %q, want /explicit/config.json", got)
		}
	})

	t.Run("没有 CONFIG_FILE 时用 BASE_DIR", func(t *testing.T) {
		t.Setenv("CHATGPT2API_CONFIG_FILE", "")
		t.Setenv("CHATGPT2API_BASE_DIR", baseDir)
		want := filepath.Join(baseDir, "config.json")
		if got := initProxyRuntimeConfigPath(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("两者都为空时不能返回空路径", func(t *testing.T) {
		t.Setenv("CHATGPT2API_CONFIG_FILE", "")
		t.Setenv("CHATGPT2API_BASE_DIR", "")
		got := initProxyRuntimeConfigPath()
		if got == "" {
			t.Fatal("返回空路径 —— 会导致 `write config : open : no such file` 这种无指向性的错误")
		}
		if filepath.Base(got) != "config.json" {
			t.Errorf("got %q, want a path ending in config.json", got)
		}
	})
}

// InitProxyRuntimeAt 必须写到给定路径（而不是别处）。
func TestInitProxyRuntimeRespectsBaseDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"auth-key":"k"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := InitProxyRuntimeAt(cfgPath)
	if err != nil {
		t.Fatalf("InitProxyRuntimeAt: %v", err)
	}
	if !changed {
		t.Fatal("没有写入任何变更")
	}
	payload, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		t.Fatal(err)
	}
	pr, ok := data["proxy_runtime"].(map[string]any)
	if !ok {
		t.Fatal("proxy_runtime 未写入")
	}
	if pr["egress_mode"] == nil {
		t.Error("egress_mode 未写入")
	}
}
