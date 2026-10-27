package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// --init-proxy-config 必须尊重 --base-dir。
//
// 回归点：RunInitProxyRuntime 只认 CHATGPT2API_CONFIG_FILE 环境变量，
// 否则硬编码 /app/config.json —— 完全忽略 --base-dir。
// 直接跑二进制（非容器）时，`--base-dir /srv/chatgpt2api --init-proxy-config`
// 会静默写到 /app/config.json（或因为目录不存在而失败），
// 而用户指定的目录里什么都没变。
func TestInitProxyRuntimeRespectsBaseDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"auth-key":"k"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// 清掉可能干扰的环境变量
	t.Setenv("CHATGPT2API_CONFIG_FILE", "")
	os.Unsetenv("CHATGPT2API_CONFIG_FILE")

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
