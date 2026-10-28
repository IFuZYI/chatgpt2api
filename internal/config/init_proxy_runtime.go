package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// 本文件是归档分支 archive/python-backend-2.0 上 scripts/init_proxy_config.py
// 的 Go 移植。docker-compose.warp.yml 的 init-config 一次性容器依赖它：
// 把 WARP/FlareSolverr 的 proxy_runtime 默认值幂等合并进 config.json，
// 这样 WARP 部署路径不再需要 Python 运行时。
//
// 与 Python 脚本逐条对应的语义：
//   - 配置文件缺失不报错，从空对象开始并创建；
//   - JSON 损坏或根不是对象 → stderr + 退出码 1；
//   - proxy_runtime 看起来还是仓库默认值（或 FORCE=1）→ 整体替换为环境变量推导值；
//   - 否则只补齐缺失的键（嵌套对象递归）；
//   - 只有内容变化才落盘；成功（含未变化）退出码 0。

const (
	initProxyConfigDefaultPath = "/app/config.json"

	initProxyDefaultEgressMode    = "single_proxy"
	initProxyDefaultProxyURL      = "http://privoxy:8118"
	initProxyDefaultClearanceMode = "flaresolverr"
	initProxyDefaultFlareSolverr  = "http://flaresolverr:8191"
	initProxyDefaultBrowser       = "chrome"
	initProxyTimeoutSec           = 60
	initProxyRefreshInterval      = 3600
	initProxyFallbackStatusCode   = 403
)

// Python: re.sub(r"(https?://)([^\s/@:]+):([^\s/@]+)@", r"\1[REDACTED]@", value, re.I)
var initProxyURLUserInfo = regexp.MustCompile(`(?i)(https?://)([^\s/@:]+):([^\s/@]+)@`)

// RunInitProxyRuntime 是 `chatgpt2api --init-proxy-config` 的入口，
// 返回进程退出码：成功（包括「已配置、无变化」）0，配置无法解析/写入 1。
func RunInitProxyRuntime() int {
	if _, err := InitProxyRuntime(initProxyRuntimeConfigPath()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// InitProxyRuntimeAt 按显式路径合并 proxy_runtime 默认值。
//
// 与 RunInitProxyRuntime 的区别：路径由调用方给定，不再只看
// CHATGPT2API_CONFIG_FILE / 硬编码 /app/config.json —— 这样
// `--base-dir` 才能生效（直接跑二进制时不会写错位置）。
func InitProxyRuntimeAt(path string) (bool, error) {
	return InitProxyRuntime(path)
}

// InitProxyRuntime 把环境变量推导出的 WARP/FlareSolverr proxy_runtime 默认值
// 幂等合并进 configPath，返回是否发生了写入。
func InitProxyRuntime(configPath string) (bool, error) {
	data := map[string]any{}
	info, statErr := os.Stat(configPath)
	switch {
	case statErr == nil && info.IsDir():
		return false, fmt.Errorf("read config %s: is a directory", configPath)
	case statErr == nil:
		payload, err := os.ReadFile(configPath)
		if err != nil {
			return false, fmt.Errorf("read config %s: %w", configPath, err)
		}
		var root any
		if err := json.Unmarshal(payload, &root); err != nil {
			return false, fmt.Errorf("Invalid JSON in %s: %w", configPath, err)
		}
		object, ok := root.(map[string]any)
		if !ok {
			return false, fmt.Errorf("Config root must be an object: %s", configPath)
		}
		data = object
	case os.IsNotExist(statErr):
		// Python 版同理：文件不存在只提示，不当作错误。
		fmt.Printf("Config file not found, creating %s\n", configPath)
	default:
		return false, fmt.Errorf("stat config %s: %w", configPath, statErr)
	}

	desired := warpProxyRuntimeDefaults()
	existing, _ := data["proxy_runtime"]
	changed := false
	if looksLikeRepositoryProxyRuntimeDefault(existing) || envBool("CHATGPT2API_PROXY_RUNTIME_FORCE", false) {
		data["proxy_runtime"] = desired
		changed = true
		fmt.Println("Created proxy_runtime defaults")
	} else {
		runtime, ok := existing.(map[string]any)
		if !ok {
			runtime = map[string]any{}
		}
		changed = deepFillMissingKeys(runtime, DefaultProxyRuntime)
		data["proxy_runtime"] = runtime
		fmt.Println("Proxy runtime already configured")
	}

	if changed {
		if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
			return false, fmt.Errorf("create config dir: %w", err)
		}
		payload, err := encodeInitProxyConfig(data)
		if err != nil {
			return false, fmt.Errorf("encode config: %w", err)
		}
		if err := writeFileAtomic(configPath, payload); err != nil {
			return false, fmt.Errorf("write config %s: %w", configPath, err)
		}
	}

	runtime, _ := data["proxy_runtime"].(map[string]any)
	fmt.Println(proxyRuntimeSummary(runtime))
	return changed, nil
}

// initProxyRuntimeConfigPath 决定 --init-proxy-config 读写哪个 config.json。
//
// 优先级（与主程序的 base-dir 解析保持一致，见 ResolveBaseDir）：
//
//	CHATGPT2API_CONFIG_FILE（显式指定，容器里 init-config 用它）
//	> CHATGPT2API_BASE_DIR/config.json
//	> ResolveBaseDir()/config.json（exe 旁或 cwd）
//	> /app/config.json（容器默认）
//
// 早期实现只认第一个环境变量、否则硬编码 /app/config.json，于是：
//   - 设了 CHATGPT2API_BASE_DIR 也不生效（与主程序行为不一致）；
//   - 变量存在但为空时得到空路径，报错是
//     `write config : open : no such file or directory` —— 完全不指向
//     该设哪个变量；
//   - 在非容器环境里静默往 /app 写（或写失败）。
func initProxyRuntimeConfigPath() string {
	if path := strings.TrimSpace(os.Getenv("CHATGPT2API_CONFIG_FILE")); path != "" {
		return path
	}
	if dir := strings.TrimSpace(os.Getenv("CHATGPT2API_BASE_DIR")); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	if dir := strings.TrimSpace(ResolveBaseDir()); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	return initProxyConfigDefaultPath
}

// warpProxyRuntimeDefaults 从环境变量推导 WARP/FlareSolverr 部署要写入的 proxy_runtime。
// 以 DefaultProxyRuntime 为骨架，保证两个默认值表不会各说各话。
func warpProxyRuntimeDefaults() map[string]any {
	clearanceEnabled := envBool("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_ENABLED", true)

	clearanceMode := strings.ToLower(strings.TrimSpace(
		envOrDefault("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_MODE", initProxyDefaultClearanceMode)))
	switch clearanceMode {
	case "none", "manual", "flaresolverr":
	default:
		if clearanceEnabled {
			clearanceMode = initProxyDefaultClearanceMode
		} else {
			clearanceMode = "none"
		}
	}
	// 关掉 clearance 时模式必须同步为 none，否则归一化后仍是「启用」语义。
	if !clearanceEnabled {
		clearanceMode = "none"
	}

	egressMode := strings.ToLower(strings.TrimSpace(
		envOrDefault("CHATGPT2API_PROXY_RUNTIME_EGRESS_MODE", initProxyDefaultEgressMode)))
	if egressMode != "direct" && egressMode != "single_proxy" {
		egressMode = initProxyDefaultEgressMode
	}

	userAgent := strings.TrimSpace(
		envOrDefault("CHATGPT2API_PROXY_RUNTIME_USER_AGENT", DefaultProxyRuntimeUserAgent))
	if userAgent == "" {
		userAgent = DefaultProxyRuntimeUserAgent
	}
	browser := strings.TrimSpace(
		envOrDefault("CHATGPT2API_PROXY_RUNTIME_BROWSER", initProxyDefaultBrowser))
	if browser == "" {
		browser = initProxyDefaultBrowser
	}

	runtime, _ := deepCopyJSONValue(DefaultProxyRuntime).(map[string]any)
	if runtime == nil {
		runtime = map[string]any{}
	}
	clearance, ok := runtime["clearance"].(map[string]any)
	if !ok {
		clearance = map[string]any{}
		runtime["clearance"] = clearance
	}

	runtime["enabled"] = envBool("CHATGPT2API_PROXY_RUNTIME_ENABLED", true)
	runtime["egress_mode"] = egressMode
	runtime["proxy_url"] = strings.TrimSpace(
		envOrDefault("CHATGPT2API_PROXY_RUNTIME_PROXY_URL", initProxyDefaultProxyURL))
	runtime["resource_proxy_url"] = strings.TrimSpace(
		envOrDefault("CHATGPT2API_PROXY_RUNTIME_RESOURCE_PROXY_URL", ""))
	runtime["skip_ssl_verify"] = envBool("CHATGPT2API_PROXY_RUNTIME_SKIP_SSL_VERIFY", false)
	runtime["reset_session_status_codes"] = parseProxyResetStatusCodes(
		envOrDefault("CHATGPT2API_PROXY_RUNTIME_RESET_STATUS_CODES", "403"))

	clearance["enabled"] = clearanceEnabled
	clearance["mode"] = clearanceMode
	clearance["user_agent"] = userAgent
	clearance["browser"] = browser
	clearance["flaresolverr_url"] = strings.TrimSpace(
		envOrDefault("CHATGPT2API_FLARESOLVERR_URL", initProxyDefaultFlareSolverr))
	clearance["timeout_sec"] = positiveIntOr(
		os.Getenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_TIMEOUT_SEC"), initProxyTimeoutSec, 1)
	clearance["refresh_interval"] = positiveIntOr(
		os.Getenv("CHATGPT2API_PROXY_RUNTIME_CLEARANCE_REFRESH_INTERVAL"), initProxyRefreshInterval, 60)
	clearance["warm_up_on_start"] = envBool("CHATGPT2API_PROXY_RUNTIME_WARM_UP_ON_START", false)
	return runtime
}

// parseProxyResetStatusCodes 复刻 Python 的列表推导：
// 逗号分隔、只保留 100..599、忽略空项/非数字、无有效项时回落 [403]，重复项保留。
func parseProxyResetStatusCodes(raw string) []any {
	codes := make([]any, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" || !isASCIIDigits(trimmed) {
			continue
		}
		code, err := strconv.Atoi(trimmed)
		if err != nil || code < 100 || code > 599 {
			continue
		}
		codes = append(codes, code)
	}
	if len(codes) == 0 {
		return []any{initProxyFallbackStatusCode}
	}
	return codes
}

func isASCIIDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// deepFillMissingKeys 只补 target 里不存在的键，递归嵌套对象；返回是否有改动。
// 已存在的值（包括类型不对的值）一律不动，和 Python 的 _deep_fill_missing 一致。
func deepFillMissingKeys(target, defaults map[string]any) bool {
	changed := false
	for key, fallback := range defaults {
		current, exists := target[key]
		if !exists {
			target[key] = deepCopyJSONValue(fallback)
			changed = true
			continue
		}
		currentMap, currentIsMap := current.(map[string]any)
		fallbackMap, fallbackIsMap := fallback.(map[string]any)
		if currentIsMap && fallbackIsMap {
			if deepFillMissingKeys(currentMap, fallbackMap) {
				changed = true
			}
		}
	}
	return changed
}

// looksLikeRepositoryProxyRuntimeDefault 判断现有 proxy_runtime 是否「还是仓库默认值」：
// 非对象视为默认；否则用默认值补齐副本后与默认值比较。
func looksLikeRepositoryProxyRuntimeDefault(value any) bool {
	existing, ok := value.(map[string]any)
	if !ok {
		return true
	}
	candidate, ok := deepCopyJSONValue(existing).(map[string]any)
	if !ok {
		return true
	}
	deepFillMissingKeys(candidate, DefaultProxyRuntime)
	return jsonValuesEqual(candidate, DefaultProxyRuntime)
}

// jsonValuesEqual 比较两份 JSON 值。config.json 反向序列化后的数字都是 float64，
// 而 DefaultProxyRuntime 里是 int，直接 reflect.DeepEqual 会误判成「不是默认值」；
// 这里按 Python 的 == 语义把数字归一化后再比。
func jsonValuesEqual(left, right any) bool {
	if leftMap, ok := left.(map[string]any); ok {
		rightMap, ok := right.(map[string]any)
		if !ok || len(leftMap) != len(rightMap) {
			return false
		}
		for key, leftValue := range leftMap {
			rightValue, exists := rightMap[key]
			if !exists || !jsonValuesEqual(leftValue, rightValue) {
				return false
			}
		}
		return true
	}
	if leftList, ok := left.([]any); ok {
		rightList, ok := right.([]any)
		if !ok || len(leftList) != len(rightList) {
			return false
		}
		for index := range leftList {
			if !jsonValuesEqual(leftList[index], rightList[index]) {
				return false
			}
		}
		return true
	}
	if leftNumber, ok := jsonNumberValue(left); ok {
		rightNumber, ok := jsonNumberValue(right)
		return ok && leftNumber == rightNumber
	}
	return left == right
}

func jsonNumberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

// deepCopyJSONValue 复制 JSON 值，避免写入时把 DefaultProxyRuntime 的嵌套对象改脏。
func deepCopyJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, item := range typed {
			copied[key] = deepCopyJSONValue(item)
		}
		return copied
	case []any:
		copied := make([]any, len(typed))
		for index, item := range typed {
			copied[index] = deepCopyJSONValue(item)
		}
		return copied
	default:
		return value
	}
}

// encodeInitProxyConfig 复刻 Python 的
// json.dumps(data, ensure_ascii=False, indent=2) + "\n"：
// 不转义 & < >，保留原样 UTF-8。
func encodeInitProxyConfig(data map[string]any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// proxyRuntimeSummary 复刻 Python 最后一行摘要（凭据打码）。
func proxyRuntimeSummary(runtime map[string]any) string {
	clearance, _ := runtime["clearance"].(map[string]any)
	enabled := "False"
	if pythonTruthy(runtime["enabled"]) {
		enabled = "True"
	}
	return fmt.Sprintf(
		"Proxy runtime summary: enabled=%s, egress_mode=%s, proxy_url=%s, "+
			"clearance_mode=%s, flaresolverr_url=%s",
		enabled,
		pythonDisplay(runtime["egress_mode"]),
		maskProxyURLUserInfo(proxyURLText(runtime["proxy_url"])),
		pythonDisplay(clearance["mode"]),
		maskProxyURLUserInfo(proxyURLText(clearance["flaresolverr_url"])),
	)
}

// proxyURLText 复刻 Python 的 `str(value or "")`。
func proxyURLText(value any) string {
	if !pythonTruthy(value) {
		return ""
	}
	return pythonDisplay(value)
}

// maskProxyURLUserInfo 复刻 Python 的 _mask_url：只替换 userinfo 里的密码。
func maskProxyURLUserInfo(value string) string {
	return initProxyURLUserInfo.ReplaceAllString(value, "${1}[REDACTED]@")
}

// pythonDisplay 让摘要行与 Python 的 str() 输出一致（True/False/None）。
func pythonDisplay(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case string:
		return typed
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	default:
		return fmt.Sprint(typed)
	}
}

func pythonTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	default:
		return true
	}
}

// envBool 与 Python 的 _env_bool 一致：空值/无法识别都回落默认值。
func envBool(name string, fallback bool) bool {
	return boolOr(os.Getenv(name), fallback)
}

// envOrDefault 与 Python 的 os.getenv(name, default) 一致：只有变量不存在才用默认值。
func envOrDefault(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}
