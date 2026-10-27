package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/config"
)

func writeConfig(t *testing.T, dir string, payload map[string]any) {
	t.Helper()
	if payload == nil {
		payload = map[string]any{"auth-key": "chatgpt2api"}
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), encoded, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func newStore(t *testing.T, payload map[string]any) (*config.Store, string) {
	t.Helper()
	dir := t.TempDir()
	writeConfig(t, dir, payload)
	t.Setenv("CHATGPT2API_AUTH_KEY", "")
	store, err := config.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store, dir
}

func TestAuthKeyFromFile(t *testing.T) {
	store, _ := newStore(t, nil)
	if got := store.AuthKey(); got != "chatgpt2api" {
		t.Fatalf("auth key = %q, want chatgpt2api", got)
	}
}

func TestAuthKeyEnvWins(t *testing.T) {
	store, _ := newStore(t, nil)
	t.Setenv("CHATGPT2API_AUTH_KEY", "from-env")
	if got := store.AuthKey(); got != "from-env" {
		t.Fatalf("auth key = %q, want from-env", got)
	}
}

func TestMissingAuthKeyFails(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, map[string]any{})
	t.Setenv("CHATGPT2API_AUTH_KEY", "")
	if _, err := config.NewStore(dir); err == nil {
		t.Fatal("expected error when auth-key is missing")
	}
}

// 默认值必须与 Python 的 config.json 一致，否则 /api/settings 会漂移。
//
// 例外：default_upstream_model_name 从 "gpt-5-5" 改成了 "auto"。
// 那是有意偏离 Python —— gpt-5-5 已从上游 /backend-api/models 消失（实测），
// 继续用它会让 gpt-image-2 这个入口指向不存在的模型。
func TestDefaultsMatchPythonConfig(t *testing.T) {
	store, _ := newStore(t, nil)
	got := store.Get()

	cases := []struct {
		key  string
		want any
	}{
		{"refresh_account_interval_minute", 5},
		// 保留期默认值**有意偏离 Python**：Python 用 30 天，本版改成
		// 1440 分钟（= 1 天）。理由：天粒度无法表达「保留几小时」，
		// 而清理本身按时间戳判断。
		//
		// 注意 image_retention_days 现在是从分钟派生的只读值
		// （向上取整），写入时会被迁移成 image_retention_minutes。
		{"image_retention_minutes", 1440},
		{"image_retention_days", 1},
		{"image_poll_timeout_secs", 120},
		{"image_account_concurrency", 3},
		{"image_parallel_generation", true},
		{"image_remove_conversation_after_result", false},
		{"auto_remove_invalid_accounts", false},
		{"default_upstream_model_name", "auto"}, // 偏离 Python，见函数注释
		{"default_thinking_effort", "auto"},
		{"global_system_prompt", ""},
	}
	for _, tc := range cases {
		if got[tc.key] != tc.want {
			t.Errorf("%s = %#v, want %#v", tc.key, got[tc.key], tc.want)
		}
	}
	if _, exists := got["auth-key"]; exists {
		t.Error("Get() must strip auth-key")
	}
}

func TestImagePollIntervalFloor(t *testing.T) {
	store, _ := newStore(t, map[string]any{
		"auth-key":                 "chatgpt2api",
		"image_poll_interval_secs": 0.1,
	})
	if got := store.ImagePollIntervalSecs(); got != 0.5 {
		t.Fatalf("image_poll_interval_secs = %v, want 0.5", got)
	}
}

func TestNormalizeProxyRuntimeFillsDefaults(t *testing.T) {
	store, _ := newStore(t, map[string]any{"auth-key": "chatgpt2api"})
	runtime := store.ProxyRuntimeSettings()

	if runtime["egress_mode"] != "direct" {
		t.Errorf("egress_mode = %v", runtime["egress_mode"])
	}
	clearance, _ := runtime["clearance"].(map[string]any)
	if clearance["mode"] != "none" {
		t.Errorf("clearance.mode = %v", clearance["mode"])
	}
	if clearance["timeout_sec"] != 60 {
		t.Errorf("clearance.timeout_sec = %v", clearance["timeout_sec"])
	}
	if clearance["refresh_interval"] != 3600 {
		t.Errorf("clearance.refresh_interval = %v", clearance["refresh_interval"])
	}
	codes, _ := runtime["reset_session_status_codes"].([]any)
	if len(codes) != 1 || codes[0] != 403 {
		t.Errorf("reset_session_status_codes = %#v", codes)
	}
}

func TestPublicProxyRuntimeMasksCookies(t *testing.T) {
	store, _ := newStore(t, map[string]any{
		"auth-key": "chatgpt2api",
		"proxy_runtime": map[string]any{
			"clearance": map[string]any{
				"cf_cookies":   "session=secret",
				"cf_clearance": "clear-me",
			},
		},
	})
	public := store.PublicProxyRuntimeSettings()
	clearance, _ := public["clearance"].(map[string]any)
	if clearance["cf_cookies"] != "" {
		t.Error("cf_cookies must be blanked")
	}
	if clearance["cf_clearance"] != "" {
		t.Error("cf_clearance must be blanked")
	}
	if clearance["has_cf_cookies"] != true {
		t.Error("has_cf_cookies must be true")
	}
	if clearance["has_cf_clearance"] != true {
		t.Error("has_cf_clearance must be true")
	}
}

// 前端回传空 cookie 时，必须保留原值（Python 的 _existing_cf_* 机制）。
func TestUpdateKeepsExistingClearanceCookies(t *testing.T) {
	store, _ := newStore(t, map[string]any{
		"auth-key": "chatgpt2api",
		"proxy_runtime": map[string]any{
			"clearance": map[string]any{
				"cf_cookies":   "session=secret",
				"cf_clearance": "clear-me",
			},
		},
	})

	_, err := store.Update(map[string]any{
		"proxy_runtime": map[string]any{
			"enabled": true,
			"clearance": map[string]any{
				"has_cf_cookies":   true,
				"has_cf_clearance": true,
			},
		},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	runtime := store.ProxyRuntimeSettings()
	clearance, _ := runtime["clearance"].(map[string]any)
	if clearance["cf_cookies"] != "session=secret" {
		t.Errorf("cf_cookies = %v, want preserved value", clearance["cf_cookies"])
	}
	if clearance["cf_clearance"] != "clear-me" {
		t.Errorf("cf_clearance = %v, want preserved value", clearance["cf_clearance"])
	}
}

func TestUpdateRejectsIncompleteImageStorage(t *testing.T) {
	store, _ := newStore(t, nil)
	_, err := store.Update(map[string]any{
		"image_storage": map[string]any{
			"enabled":         true,
			"mode":            "webdav",
			"webdav_url":      "",
			"webdav_password": "",
		},
	})
	if err == nil {
		t.Fatal("expected validation error for missing webdav url")
	}
}

func TestLogLevelsFiltered(t *testing.T) {
	store, _ := newStore(t, map[string]any{
		"auth-key":   "chatgpt2api",
		"log_levels": []any{"debug", "TRACE", "info", 3},
	})
	got := store.LogLevels()
	if len(got) != 2 || got[0] != "debug" || got[1] != "info" {
		t.Fatalf("LogLevels = %#v", got)
	}
}

func TestThinkingEffortWhitelist(t *testing.T) {
	for input, want := range map[string]string{
		"standard": "standard",
		"EXTENDED": "extended",
		"max":      "max",
		"auto":     "auto",
		"bogus":    "auto",
		"":         "auto",
	} {
		store, _ := newStore(t, map[string]any{
			"auth-key":                "chatgpt2api",
			"default_thinking_effort": input,
		})
		if got := store.DefaultThinkingEffort(); got != want {
			t.Errorf("DefaultThinkingEffort(%q) = %q, want %q", input, got, want)
		}
	}
}
