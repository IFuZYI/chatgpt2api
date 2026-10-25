package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// setInitProxyEnvDefaults 把 WARP init 读的所有环境变量固定成「未显式配置」时的默认值，
// 避免宿主环境里残留的变量影响断言。
func setInitProxyEnvDefaults(t *testing.T) {
	t.Helper()
	t.Setenv("CHATGPT2API_CONFIG_FILE", "")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_ENABLED", "true")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_EGRESS_MODE", "single_proxy")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_PROXY_URL", "http://privoxy:8118")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_RESOURCE_PROXY_URL", "")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_SKIP_SSL_VERIFY", "false")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_RESET_STATUS_CODES", "403")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_ENABLED", "true")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_MODE", "flaresolverr")
	t.Setenv("CHATGPT2API_FLARESOLVERR_URL", "http://flaresolverr:8191")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_TIMEOUT_SEC", "60")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_REFRESH_INTERVAL", "3600")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_WARM_UP_ON_START", "false")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_BROWSER", "chrome")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_USER_AGENT", "")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_FORCE", "false")
}

func writeInitProxyConfigFile(t *testing.T, path string, payload map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal config fixture: %v", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
}

func readInitProxyConfigFile(t *testing.T, path string) map[string]any {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		t.Fatalf("parse config: %v\n%s", err, payload)
	}
	return data
}

func initProxyRuntimeObject(t *testing.T, data map[string]any) map[string]any {
	t.Helper()
	runtime, ok := data["proxy_runtime"].(map[string]any)
	if !ok {
		t.Fatalf("proxy_runtime missing or not an object: %#v", data["proxy_runtime"])
	}
	return runtime
}

func initProxyClearanceObject(t *testing.T, runtime map[string]any) map[string]any {
	t.Helper()
	clearance, ok := runtime["clearance"].(map[string]any)
	if !ok {
		t.Fatalf("clearance missing or not an object: %#v", runtime["clearance"])
	}
	return clearance
}

func assertInitProxyValue(t *testing.T, label string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", label, got, want)
	}
}

func cloneInitProxyDefaultRuntime(t *testing.T) map[string]any {
	t.Helper()
	cloned, ok := deepCopyJSONValue(DefaultProxyRuntime).(map[string]any)
	if !ok {
		t.Fatal("DefaultProxyRuntime is not an object")
	}
	return cloned
}

func TestInitProxyRuntimeCreatesMissingFile(t *testing.T) {
	setInitProxyEnvDefaults(t)
	configPath := filepath.Join(t.TempDir(), "nested", "config.json")

	changed, err := InitProxyRuntime(configPath)
	if err != nil {
		t.Fatalf("InitProxyRuntime: %v", err)
	}
	if !changed {
		t.Fatal("expected a write when the config file is absent")
	}

	data := readInitProxyConfigFile(t, configPath)
	runtime := initProxyRuntimeObject(t, data)
	clearance := initProxyClearanceObject(t, runtime)

	assertInitProxyValue(t, "enabled", runtime["enabled"], true)
	assertInitProxyValue(t, "egress_mode", runtime["egress_mode"], "single_proxy")
	assertInitProxyValue(t, "proxy_url", runtime["proxy_url"], "http://privoxy:8118")
	assertInitProxyValue(t, "resource_proxy_url", runtime["resource_proxy_url"], "")
	assertInitProxyValue(t, "skip_ssl_verify", runtime["skip_ssl_verify"], false)
	assertInitProxyValue(t, "reset_session_status_codes", runtime["reset_session_status_codes"],
		[]any{float64(403)})

	assertInitProxyValue(t, "clearance.enabled", clearance["enabled"], true)
	assertInitProxyValue(t, "clearance.mode", clearance["mode"], "flaresolverr")
	assertInitProxyValue(t, "clearance.user_agent", clearance["user_agent"], DefaultProxyRuntimeUserAgent)
	assertInitProxyValue(t, "clearance.browser", clearance["browser"], "chrome")
	assertInitProxyValue(t, "clearance.flaresolverr_url", clearance["flaresolverr_url"],
		"http://flaresolverr:8191")
	assertInitProxyValue(t, "clearance.timeout_sec", clearance["timeout_sec"], float64(60))
	assertInitProxyValue(t, "clearance.refresh_interval", clearance["refresh_interval"], float64(3600))
	assertInitProxyValue(t, "clearance.warm_up_on_start", clearance["warm_up_on_start"], false)
}

func TestInitProxyRuntimePreservesCustomizedRuntime(t *testing.T) {
	setInitProxyEnvDefaults(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeInitProxyConfigFile(t, configPath, map[string]any{
		"auth-key": "keep-me",
		"proxy_runtime": map[string]any{
			"enabled":     false,
			"egress_mode": "direct",
			"proxy_url":   "http://custom-proxy:9000",
			"clearance": map[string]any{
				"enabled":    true,
				"mode":       "manual",
				"cf_cookies": "session=keep",
			},
		},
	})

	changed, err := InitProxyRuntime(configPath)
	if err != nil {
		t.Fatalf("InitProxyRuntime: %v", err)
	}
	if !changed {
		t.Fatal("expected missing keys to be filled into the customized runtime")
	}

	data := readInitProxyConfigFile(t, configPath)
	assertInitProxyValue(t, "auth-key", data["auth-key"], "keep-me")
	runtime := initProxyRuntimeObject(t, data)
	// 已有的自定义值必须原样保留，不能被 WARP 的环境变量覆盖。
	assertInitProxyValue(t, "enabled", runtime["enabled"], false)
	assertInitProxyValue(t, "egress_mode", runtime["egress_mode"], "direct")
	assertInitProxyValue(t, "proxy_url", runtime["proxy_url"], "http://custom-proxy:9000")

	clearance := initProxyClearanceObject(t, runtime)
	assertInitProxyValue(t, "clearance.enabled", clearance["enabled"], true)
	assertInitProxyValue(t, "clearance.mode", clearance["mode"], "manual")
	assertInitProxyValue(t, "clearance.cf_cookies", clearance["cf_cookies"], "session=keep")
	// 缺失键按仓库默认值补齐（不是 WARP 的环境变量值）。
	assertInitProxyValue(t, "resource_proxy_url", runtime["resource_proxy_url"], "")
	assertInitProxyValue(t, "skip_ssl_verify", runtime["skip_ssl_verify"], false)
	assertInitProxyValue(t, "clearance.user_agent", clearance["user_agent"], DefaultProxyRuntimeUserAgent)
	assertInitProxyValue(t, "clearance.browser", clearance["browser"], "chrome")
	assertInitProxyValue(t, "clearance.flaresolverr_url", clearance["flaresolverr_url"], "")
	assertInitProxyValue(t, "clearance.timeout_sec", clearance["timeout_sec"], float64(60))
	assertInitProxyValue(t, "clearance.refresh_interval", clearance["refresh_interval"], float64(3600))
	assertInitProxyValue(t, "clearance.warm_up_on_start", clearance["warm_up_on_start"], false)

	firstWrite, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after first run: %v", err)
	}
	changed, err = InitProxyRuntime(configPath)
	if err != nil {
		t.Fatalf("second InitProxyRuntime: %v", err)
	}
	if changed {
		t.Fatal("second run must not rewrite an already filled runtime")
	}
	secondWrite, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after second run: %v", err)
	}
	if !bytes.Equal(firstWrite, secondWrite) {
		t.Fatal("second run changed the config file")
	}
}

func TestInitProxyRuntimeReplacesRepositoryDefault(t *testing.T) {
	cases := []struct {
		name         string
		proxyRuntime any
	}{
		{name: "exact repository default", proxyRuntime: cloneInitProxyDefaultRuntime(t)},
		{name: "partial default", proxyRuntime: map[string]any{"enabled": false}},
		{name: "non-object", proxyRuntime: "legacy-string"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			setInitProxyEnvDefaults(t)
			configPath := filepath.Join(t.TempDir(), "config.json")
			writeInitProxyConfigFile(t, configPath, map[string]any{
				"auth-key":      "keep-me",
				"proxy_runtime": testCase.proxyRuntime,
			})

			changed, err := InitProxyRuntime(configPath)
			if err != nil {
				t.Fatalf("InitProxyRuntime: %v", err)
			}
			if !changed {
				t.Fatal("expected the repository default to be replaced")
			}

			data := readInitProxyConfigFile(t, configPath)
			assertInitProxyValue(t, "auth-key", data["auth-key"], "keep-me")
			runtime := initProxyRuntimeObject(t, data)
			clearance := initProxyClearanceObject(t, runtime)
			assertInitProxyValue(t, "enabled", runtime["enabled"], true)
			assertInitProxyValue(t, "egress_mode", runtime["egress_mode"], "single_proxy")
			assertInitProxyValue(t, "proxy_url", runtime["proxy_url"], "http://privoxy:8118")
			assertInitProxyValue(t, "clearance.enabled", clearance["enabled"], true)
			assertInitProxyValue(t, "clearance.mode", clearance["mode"], "flaresolverr")
			assertInitProxyValue(t, "clearance.flaresolverr_url", clearance["flaresolverr_url"],
				"http://flaresolverr:8191")
		})
	}
}

func TestInitProxyRuntimeForceReplacesCustomized(t *testing.T) {
	setInitProxyEnvDefaults(t)
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_FORCE", "1")
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeInitProxyConfigFile(t, configPath, map[string]any{
		"auth-key": "keep-me",
		"proxy_runtime": map[string]any{
			"enabled":                    false,
			"egress_mode":                "direct",
			"proxy_url":                  "http://custom-proxy:9000",
			"skip_ssl_verify":            true,
			"reset_session_status_codes": []any{500},
			"clearance": map[string]any{
				"enabled":          false,
				"mode":             "none",
				"cf_cookies":       "session=stale",
				"user_agent":       "custom-ua",
				"browser":          "firefox",
				"timeout_sec":      5,
				"warm_up_on_start": true,
			},
		},
	})

	changed, err := InitProxyRuntime(configPath)
	if err != nil {
		t.Fatalf("InitProxyRuntime: %v", err)
	}
	if !changed {
		t.Fatal("FORCE must replace a customized runtime")
	}

	data := readInitProxyConfigFile(t, configPath)
	assertInitProxyValue(t, "auth-key", data["auth-key"], "keep-me")
	runtime := initProxyRuntimeObject(t, data)
	clearance := initProxyClearanceObject(t, runtime)
	assertInitProxyValue(t, "enabled", runtime["enabled"], true)
	assertInitProxyValue(t, "egress_mode", runtime["egress_mode"], "single_proxy")
	assertInitProxyValue(t, "proxy_url", runtime["proxy_url"], "http://privoxy:8118")
	assertInitProxyValue(t, "skip_ssl_verify", runtime["skip_ssl_verify"], false)
	assertInitProxyValue(t, "reset_session_status_codes", runtime["reset_session_status_codes"],
		[]any{float64(403)})
	assertInitProxyValue(t, "clearance.enabled", clearance["enabled"], true)
	assertInitProxyValue(t, "clearance.mode", clearance["mode"], "flaresolverr")
	assertInitProxyValue(t, "clearance.cf_cookies", clearance["cf_cookies"], "")
	assertInitProxyValue(t, "clearance.user_agent", clearance["user_agent"], DefaultProxyRuntimeUserAgent)
	assertInitProxyValue(t, "clearance.browser", clearance["browser"], "chrome")
	assertInitProxyValue(t, "clearance.timeout_sec", clearance["timeout_sec"], float64(60))
	assertInitProxyValue(t, "clearance.warm_up_on_start", clearance["warm_up_on_start"], false)
}

func TestInitProxyRuntimeRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantErr string
	}{
		{name: "invalid json", payload: "{not json", wantErr: "Invalid JSON in"},
		{name: "empty file", payload: "", wantErr: "Invalid JSON in"},
		{name: "non-object root", payload: "[]\n", wantErr: "Config root must be an object"},
		{name: "scalar root", payload: "42\n", wantErr: "Config root must be an object"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			setInitProxyEnvDefaults(t)
			configPath := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(configPath, []byte(testCase.payload), 0o644); err != nil {
				t.Fatalf("write config fixture: %v", err)
			}

			changed, err := InitProxyRuntime(configPath)
			if err == nil {
				t.Fatal("expected an error")
			}
			if changed {
				t.Fatal("must not report a write when the config is unusable")
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, testCase.wantErr)
			}

			payload, readErr := os.ReadFile(configPath)
			if readErr != nil {
				t.Fatalf("read config: %v", readErr)
			}
			if string(payload) != testCase.payload {
				t.Fatalf("config file was modified: %q", payload)
			}
		})
	}
}

func TestInitProxyRuntimeRunExitCodes(t *testing.T) {
	setInitProxyEnvDefaults(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("CHATGPT2API_CONFIG_FILE", configPath)

	if code := RunInitProxyRuntime(); code != 0 {
		t.Fatalf("first run exit code = %d, want 0", code)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file was not created: %v", err)
	}
	firstWrite, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	if code := RunInitProxyRuntime(); code != 0 {
		t.Fatalf("second run exit code = %d, want 0", code)
	}
	secondWrite, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !bytes.Equal(firstWrite, secondWrite) {
		t.Fatal("second run changed the config file")
	}

	invalidPath := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(invalidPath, []byte("{"), 0o644); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	t.Setenv("CHATGPT2API_CONFIG_FILE", invalidPath)
	if code := RunInitProxyRuntime(); code != 1 {
		t.Fatalf("invalid config exit code = %d, want 1", code)
	}
}

func TestInitProxyRuntimeResetStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []any
	}{
		{name: "mixed", raw: "403,999,abc,500", want: []any{float64(403), float64(500)}},
		{name: "all invalid", raw: "999", want: []any{float64(403)}},
		{name: "empty", raw: "", want: []any{float64(403)}},
		{name: "whitespace and bounds", raw: " 100 , 599 ", want: []any{float64(100), float64(599)}},
		{name: "out of bounds", raw: "99,600", want: []any{float64(403)}},
		{name: "duplicates kept", raw: "403,403", want: []any{float64(403), float64(403)}},
		{name: "signed value rejected", raw: "+403", want: []any{float64(403)}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			setInitProxyEnvDefaults(t)
			t.Setenv("CHATGPT2API_PROXY_RUNTIME_RESET_STATUS_CODES", testCase.raw)
			configPath := filepath.Join(t.TempDir(), "config.json")

			if _, err := InitProxyRuntime(configPath); err != nil {
				t.Fatalf("InitProxyRuntime: %v", err)
			}
			runtime := initProxyRuntimeObject(t, readInitProxyConfigFile(t, configPath))
			assertInitProxyValue(t, "reset_session_status_codes",
				runtime["reset_session_status_codes"], testCase.want)
		})
	}
}

func TestInitProxyRuntimeClearanceModes(t *testing.T) {
	cases := []struct {
		name        string
		enabled     string
		mode        string
		wantEnabled bool
		wantMode    string
	}{
		{name: "disabled forces none", enabled: "false", mode: "flaresolverr", wantEnabled: false, wantMode: "none"},
		{name: "disabled with invalid mode", enabled: "false", mode: "bogus", wantEnabled: false, wantMode: "none"},
		{name: "invalid mode falls back to flaresolverr", enabled: "true", mode: "bogus", wantEnabled: true, wantMode: "flaresolverr"},
		{name: "empty mode falls back to flaresolverr", enabled: "true", mode: "", wantEnabled: true, wantMode: "flaresolverr"},
		{name: "manual kept", enabled: "true", mode: "manual", wantEnabled: true, wantMode: "manual"},
		{name: "none kept", enabled: "true", mode: "none", wantEnabled: true, wantMode: "none"},
		{name: "uppercase lowercased", enabled: "true", mode: "FLARESOLVERR", wantEnabled: true, wantMode: "flaresolverr"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			setInitProxyEnvDefaults(t)
			t.Setenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_ENABLED", testCase.enabled)
			t.Setenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_MODE", testCase.mode)
			configPath := filepath.Join(t.TempDir(), "config.json")

			if _, err := InitProxyRuntime(configPath); err != nil {
				t.Fatalf("InitProxyRuntime: %v", err)
			}
			runtime := initProxyRuntimeObject(t, readInitProxyConfigFile(t, configPath))
			clearance := initProxyClearanceObject(t, runtime)
			assertInitProxyValue(t, "clearance.enabled", clearance["enabled"], testCase.wantEnabled)
			assertInitProxyValue(t, "clearance.mode", clearance["mode"], testCase.wantMode)
		})
	}
}

func TestInitProxyRuntimeEgressModeFallback(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{raw: "bogus", want: "single_proxy"},
		{raw: "", want: "single_proxy"},
		{raw: "DIRECT", want: "direct"},
		{raw: "direct", want: "direct"},
		{raw: " single_proxy ", want: "single_proxy"},
	}
	for _, testCase := range cases {
		t.Run("egress="+testCase.raw, func(t *testing.T) {
			setInitProxyEnvDefaults(t)
			t.Setenv("CHATGPT2API_PROXY_RUNTIME_EGRESS_MODE", testCase.raw)
			configPath := filepath.Join(t.TempDir(), "config.json")

			if _, err := InitProxyRuntime(configPath); err != nil {
				t.Fatalf("InitProxyRuntime: %v", err)
			}
			runtime := initProxyRuntimeObject(t, readInitProxyConfigFile(t, configPath))
			assertInitProxyValue(t, "egress_mode", runtime["egress_mode"], testCase.want)
		})
	}
}

func TestInitProxyRuntimeAppliesEnvOverrides(t *testing.T) {
	setInitProxyEnvDefaults(t)
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_ENABLED", "false")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_EGRESS_MODE", "direct")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_PROXY_URL", "http://user:secret@proxy.example:8118")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_RESOURCE_PROXY_URL", "http://resource-proxy:8118")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_SKIP_SSL_VERIFY", "yes")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_RESET_STATUS_CODES", "429,500")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_ENABLED", "true")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_MODE", "manual")
	t.Setenv("CHATGPT2API_FLARESOLVERR_URL", "http://flare-user:flare-pass@flaresolverr:8191")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_TIMEOUT_SEC", "5")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_REFRESH_INTERVAL", "120")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_WARM_UP_ON_START", "true")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_BROWSER", "firefox")
	t.Setenv("CHATGPT2API_PROXY_RUNTIME_USER_AGENT", "custom-ua")
	configPath := filepath.Join(t.TempDir(), "config.json")

	if _, err := InitProxyRuntime(configPath); err != nil {
		t.Fatalf("InitProxyRuntime: %v", err)
	}
	runtime := initProxyRuntimeObject(t, readInitProxyConfigFile(t, configPath))
	clearance := initProxyClearanceObject(t, runtime)

	assertInitProxyValue(t, "enabled", runtime["enabled"], false)
	assertInitProxyValue(t, "egress_mode", runtime["egress_mode"], "direct")
	assertInitProxyValue(t, "proxy_url", runtime["proxy_url"],
		"http://user:secret@proxy.example:8118")
	assertInitProxyValue(t, "resource_proxy_url", runtime["resource_proxy_url"],
		"http://resource-proxy:8118")
	assertInitProxyValue(t, "skip_ssl_verify", runtime["skip_ssl_verify"], true)
	assertInitProxyValue(t, "reset_session_status_codes", runtime["reset_session_status_codes"],
		[]any{float64(429), float64(500)})
	assertInitProxyValue(t, "clearance.enabled", clearance["enabled"], true)
	assertInitProxyValue(t, "clearance.mode", clearance["mode"], "manual")
	assertInitProxyValue(t, "clearance.user_agent", clearance["user_agent"], "custom-ua")
	assertInitProxyValue(t, "clearance.browser", clearance["browser"], "firefox")
	assertInitProxyValue(t, "clearance.flaresolverr_url", clearance["flaresolverr_url"],
		"http://flare-user:flare-pass@flaresolverr:8191")
	assertInitProxyValue(t, "clearance.timeout_sec", clearance["timeout_sec"], float64(5))
	assertInitProxyValue(t, "clearance.refresh_interval", clearance["refresh_interval"], float64(120))
	assertInitProxyValue(t, "clearance.warm_up_on_start", clearance["warm_up_on_start"], true)
}

func TestInitProxyRuntimeClearanceIntFloors(t *testing.T) {
	cases := []struct {
		name        string
		timeoutRaw  string
		refreshRaw  string
		wantTimeout float64
		wantRefresh float64
	}{
		{name: "below floor clamps", timeoutRaw: "0", refreshRaw: "59", wantTimeout: 1, wantRefresh: 60},
		{name: "negative clamps", timeoutRaw: "-5", refreshRaw: "-1", wantTimeout: 1, wantRefresh: 60},
		{name: "invalid falls back", timeoutRaw: "abc", refreshRaw: "abc", wantTimeout: 60, wantRefresh: 3600},
		{name: "valid kept", timeoutRaw: "90", refreshRaw: "7200", wantTimeout: 90, wantRefresh: 7200},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			setInitProxyEnvDefaults(t)
			t.Setenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_TIMEOUT_SEC", testCase.timeoutRaw)
			t.Setenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_REFRESH_INTERVAL", testCase.refreshRaw)
			configPath := filepath.Join(t.TempDir(), "config.json")

			if _, err := InitProxyRuntime(configPath); err != nil {
				t.Fatalf("InitProxyRuntime: %v", err)
			}
			runtime := initProxyRuntimeObject(t, readInitProxyConfigFile(t, configPath))
			clearance := initProxyClearanceObject(t, runtime)
			assertInitProxyValue(t, "clearance.timeout_sec", clearance["timeout_sec"], testCase.wantTimeout)
			assertInitProxyValue(t, "clearance.refresh_interval", clearance["refresh_interval"],
				testCase.wantRefresh)
		})
	}
}

func TestInitProxyMaskURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "plain url intact", raw: "http://privoxy:8118", want: "http://privoxy:8118"},
		{name: "https credentials masked", raw: "https://user:secret@example.com:8443",
			want: "https://[REDACTED]@example.com:8443"},
		{name: "case insensitive scheme", raw: "HTTP://User:Pass@Host:8080",
			want: "HTTP://[REDACTED]@Host:8080"},
		{name: "username only untouched", raw: "http://user@host:8080", want: "http://user@host:8080"},
		{name: "non http scheme untouched", raw: "socks5://user:pass@host:1080",
			want: "socks5://user:pass@host:1080"},
		{name: "empty", raw: "", want: ""},
		{name: "password stops at at sign", raw: "http://user:p@ss@host",
			want: "http://[REDACTED]@ss@host"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := maskProxyURLUserInfo(testCase.raw); got != testCase.want {
				t.Fatalf("maskProxyURLUserInfo(%q) = %q, want %q", testCase.raw, got, testCase.want)
			}
		})
	}
}

func TestInitProxySummaryMasksCredentials(t *testing.T) {
	runtime := map[string]any{
		"enabled":     true,
		"egress_mode": "single_proxy",
		"proxy_url":   "http://user:secret@privoxy:8118",
		"clearance": map[string]any{
			"mode":             "flaresolverr",
			"flaresolverr_url": "http://flare:token@flaresolverr:8191",
		},
	}
	want := "Proxy runtime summary: enabled=True, egress_mode=single_proxy, " +
		"proxy_url=http://[REDACTED]@privoxy:8118, clearance_mode=flaresolverr, " +
		"flaresolverr_url=http://[REDACTED]@flaresolverr:8191"
	if got := proxyRuntimeSummary(runtime); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}
