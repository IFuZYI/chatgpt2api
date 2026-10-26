package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// 默认值。这些字面量必须与 Python 版 services/config.py 中的
// DEFAULT_* 常量逐字一致，因为它们是 config.json 归一化的输入。
var (
	DefaultBackupInclude = map[string]bool{
		"config":             true,
		"cpa":                true,
		"sub2api":            true,
		"logs":               true,
		"image_tasks":        true,
		"accounts_snapshot":  true,
		"auth_keys_snapshot": true,
		"images":             false,
	}

	DefaultProxyRuntimeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) " +
		"Chrome/145.0.0.0 Safari/537.36"

	DefaultChatCompletionCache = map[string]any{
		"enabled":                  true,
		"ttl_seconds":              60,
		"max_entries":              256,
		"dedupe_inflight":          true,
		"stream_cache":             true,
		"normalize_messages":       true,
		"drop_adjacent_duplicates": true,
		"drop_assistant_history":   false,
	}

	DefaultImageStorage = map[string]any{
		"enabled":          false,
		"mode":             "local",
		"webdav_url":       "",
		"webdav_username":  "",
		"webdav_password":  "",
		"webdav_root_path": "chatgpt2api/images",
		"public_base_url":  "",
	}

	DefaultProxyRuntime = map[string]any{
		"enabled":                    false,
		"egress_mode":                "direct",
		"proxy_url":                  "",
		"resource_proxy_url":         "",
		"skip_ssl_verify":            false,
		"reset_session_status_codes": []any{403},
		"clearance": map[string]any{
			"enabled":          false,
			"mode":             "none",
			"cf_cookies":       "",
			"cf_clearance":     "",
			"user_agent":       DefaultProxyRuntimeUserAgent,
			"browser":          "chrome",
			"flaresolverr_url": "",
			"timeout_sec":      60,
			"refresh_interval": 3600,
			"warm_up_on_start": false,
		},
	}
)

// Store 是 config.json 的内存副本 + 归一化访问器。
// 所有写操作落盘后再更新内存，与 Python 的 update() 顺序一致。
type Store struct {
	mu   sync.RWMutex
	path string
	data map[string]any

	baseDir string
	dataDir string
}

// ResolveBaseDir 决定项目根目录。
// 优先级：CHATGPT2API_BASE_DIR > 可执行文件所在目录（若含 config.json）> 当前工作目录。
func ResolveBaseDir() string {
	if dir := strings.TrimSpace(os.Getenv("CHATGPT2API_BASE_DIR")); dir != "" {
		return dir
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
			return dir
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}

// NewStore 加载 config.json。auth-key 缺失时报错，与 Python 的 ConfigStore 行为一致。
func NewStore(baseDir string) (*Store, error) {
	dataDir := filepath.Join(baseDir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(baseDir, "config.json")
	raw, err := readJSONObject(path)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, data: raw, baseDir: baseDir, dataDir: dataDir}
	if s.AuthKey() == "" {
		return nil, errors.New("❌ auth-key 未设置！\n请在环境变量 CHATGPT2API_AUTH_KEY 中设置，或者在 config.json 中填写 auth-key。")
	}
	return s, nil
}

func readJSONObject(path string) (map[string]any, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if info.IsDir() {
		// Python 版这里只警告并回退，不中断启动。
		fmt.Fprintf(os.Stderr,
			"Warning: config at '%s' is a directory, ignoring it and falling back to other configuration sources.\n", path)
		return map[string]any{}, nil
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		// Python 版对解析失败静默回退为空对象。
		return map[string]any{}, nil
	}
	return out, nil
}

func (s *Store) BaseDir() string { return s.baseDir }
func (s *Store) DataDir() string { return s.dataDir }
func (s *Store) Path() string    { return s.path }

func (s *Store) AccountsFile() string { return filepath.Join(s.dataDir, "accounts.json") }
func (s *Store) AuthKeysFile() string { return filepath.Join(s.dataDir, "auth_keys.json") }
func (s *Store) BackupStateFile() string {
	return filepath.Join(s.dataDir, "backup_state.json")
}

// AuthKey 环境变量优先于 config.json。
func (s *Store) AuthKey() string {
	if env := strings.TrimSpace(os.Getenv("CHATGPT2API_AUTH_KEY")); env != "" {
		return env
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, _ := s.data["auth-key"].(string)
	return strings.TrimSpace(value)
}

// Snapshot 返回原始配置的浅拷贝，调用方可安全读取。
func (s *Store) Snapshot() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]any, len(s.data))
	for key, value := range s.data {
		out[key] = value
	}
	return out
}

func (s *Store) raw(key string) any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data[key]
}

// ---- 标量访问器 ----

func (s *Store) RefreshAccountIntervalMinute() int {
	return positiveIntOr(s.raw("refresh_account_interval_minute"), 5, 0)
}

func (s *Store) ImageRetentionDays() int {
	value := positiveIntOr(s.raw("image_retention_days"), 30, 1)
	return value
}

func (s *Store) ImagePollTimeoutSecs() int {
	return positiveIntOr(s.raw("image_poll_timeout_secs"), 120, 1)
}

func (s *Store) ImagePollIntervalSecs() float64 {
	return maxFloat(floatOr(s.raw("image_poll_interval_secs"), 10.0), 0.5)
}

func (s *Store) ImagePollInitialWaitSecs() float64 {
	return maxFloat(floatOr(s.raw("image_poll_initial_wait_secs"), 10.0), 0.0)
}

func (s *Store) ImageAccountConcurrency() int {
	return positiveIntOr(s.raw("image_account_concurrency"), 3, 1)
}

func (s *Store) ImageParallelGeneration() bool {
	return boolOr(s.raw("image_parallel_generation"), true)
}

func (s *Store) ImageSettleEnabled() bool {
	return boolOr(s.raw("image_settle_enabled"), true)
}

func (s *Store) ImageCheckBeforeHitEnabled() bool {
	return boolOr(s.raw("image_check_before_hit_enabled"), true)
}

func (s *Store) ImageRemoveConversationAfterResult() bool {
	return boolOr(s.raw("image_remove_conversation_after_result"), false)
}

func (s *Store) ImageRemoveConversationAlways() bool {
	return boolOr(s.raw("image_remove_conversation_always"), false)
}

func (s *Store) ImageSettleSecs() float64 {
	return maxFloat(floatOr(s.raw("image_settle_secs"), 2.0), 0.5)
}

func (s *Store) ImageMinFreeMB() int {
	return positiveIntOr(s.raw("image_min_free_mb"), 500, 0)
}

func (s *Store) AutoRemoveInvalidAccounts() bool {
	return boolOr(s.raw("auto_remove_invalid_accounts"), false)
}

func (s *Store) AutoRemoveRateLimitedAccounts() bool {
	return boolOr(s.raw("auto_remove_rate_limited_accounts"), false)
}

func (s *Store) AutoReloginAfterRefresh() bool {
	return boolOr(s.raw("auto_relogin_after_refresh"), false)
}

func (s *Store) GlobalSystemPrompt() string {
	value, _ := s.raw("global_system_prompt").(string)
	return strings.TrimSpace(value)
}

func (s *Store) DefaultUpstreamModelName() string {
	value, _ := s.raw("default_upstream_model_name").(string)
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	// 回退值用 auto：gpt-5-5 已从上游 /backend-api/models 消失
	// （实测列表里没有它），继续用它会让 gpt-image-2 这个入口指向一个
	// 不存在的模型。auto 是上游始终提供的兜底项。
	return "auto"
}

func (s *Store) DefaultThinkingEffort() string {
	value, _ := s.raw("default_thinking_effort").(string)
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "standard", "extended", "max", "auto":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "auto"
	}
}

func (s *Store) LogLevels() []string {
	raw, ok := s.raw("log_levels").([]any)
	if !ok {
		return []string{}
	}
	allowed := map[string]bool{"debug": true, "info": true, "warning": true, "error": true}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		text, _ := item.(string)
		level := strings.ToLower(strings.TrimSpace(text))
		if allowed[level] {
			out = append(out, level)
		}
	}
	return out
}

func (s *Store) SensitiveWords() []string {
	raw, ok := s.raw("sensitive_words").([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		text, _ := item.(string)
		if word := strings.TrimSpace(text); word != "" {
			out = append(out, word)
		}
	}
	return out
}

// AIReview 返回 ai_review 配置的**副本**。
//
// 必须复制：s.raw 返回的是 s.data 里的活 map，直接给出去后调用方
// 改它就会绕过 mu 修改内部状态，与并发读者竞争。
func (s *Store) AIReview() map[string]any {
	if value, ok := s.raw("ai_review").(map[string]any); ok {
		return deepCopyMap(value)
	}
	return map[string]any{}
}

func (s *Store) Proxy() string {
	value, _ := s.raw("proxy").(string)
	return strings.TrimSpace(value)
}

// BaseURL 环境变量优先。
func (s *Store) BaseURL() string {
	if env := strings.TrimSpace(os.Getenv("CHATGPT2API_BASE_URL")); env != "" {
		return strings.TrimRight(env, "/")
	}
	value, _ := s.raw("base_url").(string)
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

// BuildVersion 由构建期注入：
//
//	go build -ldflags "-X github.com/IFuZYI/chatgpt2api/internal/config.BuildVersion=2.0.0"
//
// 单二进制发布（GitHub Release）没有 VERSION 文件可读，靠它兜底。
var BuildVersion = ""

// AppVersion 优先读 baseDir 下的 VERSION 文件（Docker 镜像与源码运行都有），
// 读不到再退回编译期注入的 BuildVersion，最后才是 "0.0.0"。
func (s *Store) AppVersion() string {
	if payload, err := os.ReadFile(filepath.Join(s.baseDir, "VERSION")); err == nil {
		if version := strings.TrimSpace(string(payload)); version != "" {
			return version
		}
	}
	if version := strings.TrimSpace(BuildVersion); version != "" {
		return version
	}
	return "0.0.0"
}

func (s *Store) ImagesDir() string {
	dir := filepath.Join(s.dataDir, "images")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func (s *Store) ImageThumbnailsDir() string {
	dir := filepath.Join(s.dataDir, "image_thumbnails")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func (s *Store) FilesDir() string {
	dir := filepath.Join(s.dataDir, "files")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// ---- 归一化子结构 ----

func (s *Store) BackupSettings() map[string]any {
	return normalizeBackupSettings(s.raw("backup"))
}

func (s *Store) ImageStorageSettings() map[string]any {
	return normalizeImageStorageSettings(s.raw("image_storage"))
}

func (s *Store) ChatCompletionCacheSettings() map[string]any {
	return normalizeChatCompletionCache(s.raw("chat_completion_cache"))
}

func (s *Store) ProxyRuntimeSettings() map[string]any {
	return normalizeProxyRuntime(s.raw("proxy_runtime"))
}

// PublicProxyRuntimeSettings 抹掉 clearance 里的敏感 cookie，只保留布尔标记。
func (s *Store) PublicProxyRuntimeSettings() map[string]any {
	runtime := deepCopyMap(s.ProxyRuntimeSettings())
	clearance, ok := runtime["clearance"].(map[string]any)
	if ok {
		cookies, _ := clearance["cf_cookies"].(string)
		cfClearance, _ := clearance["cf_clearance"].(string)
		clearance["cf_cookies"] = ""
		clearance["cf_clearance"] = ""
		clearance["has_cf_cookies"] = strings.TrimSpace(cookies) != ""
		clearance["has_cf_clearance"] = strings.TrimSpace(cfClearance) != ""
	}
	return runtime
}

// Get 复刻 Python ConfigStore.get()：原始数据 + 归一化覆盖，最后剔除 auth-key。
func (s *Store) Get() map[string]any {
	out := s.Snapshot()
	out["refresh_account_interval_minute"] = s.RefreshAccountIntervalMinute()
	out["image_retention_days"] = s.ImageRetentionDays()
	out["image_poll_timeout_secs"] = s.ImagePollTimeoutSecs()
	out["image_poll_interval_secs"] = s.ImagePollIntervalSecs()
	out["image_poll_initial_wait_secs"] = s.ImagePollInitialWaitSecs()
	out["image_account_concurrency"] = s.ImageAccountConcurrency()
	out["image_parallel_generation"] = s.ImageParallelGeneration()
	out["image_remove_conversation_after_result"] = s.ImageRemoveConversationAfterResult()
	out["image_remove_conversation_always"] = s.ImageRemoveConversationAlways()
	out["auto_remove_invalid_accounts"] = s.AutoRemoveInvalidAccounts()
	out["auto_remove_rate_limited_accounts"] = s.AutoRemoveRateLimitedAccounts()
	out["auto_relogin_after_refresh"] = s.AutoReloginAfterRefresh()
	out["log_levels"] = s.LogLevels()
	out["sensitive_words"] = s.SensitiveWords()
	out["ai_review"] = s.AIReview()
	out["global_system_prompt"] = s.GlobalSystemPrompt()
	out["default_upstream_model_name"] = s.DefaultUpstreamModelName()
	out["default_thinking_effort"] = s.DefaultThinkingEffort()
	out["backup"] = s.BackupSettings()
	out["image_storage"] = s.ImageStorageSettings()
	out["chat_completion_cache"] = s.ChatCompletionCacheSettings()
	out["proxy_runtime"] = s.PublicProxyRuntimeSettings()
	delete(out, "auth-key")
	return out
}

// Update 合并、归一化、落盘、回读。返回归一化后的完整配置。
func (s *Store) Update(patch map[string]any) (map[string]any, error) {
	s.mu.Lock()
	next := make(map[string]any, len(s.data)+len(patch))
	for key, value := range s.data {
		next[key] = value
	}
	for key, value := range patch {
		next[key] = value
	}

	if _, ok := next["backup"]; ok {
		incoming, isMap := next["backup"].(map[string]any)
		if isMap {
			incoming = preserveMaskedSecrets(incoming, s.sectionLocked("backup"),
				[]string{"secret_access_key", "passphrase"})
		}
		next["backup"] = normalizeBackupSettings(incoming)
	}
	if _, ok := next["ai_review"]; ok {
		incoming, isMap := next["ai_review"].(map[string]any)
		if isMap {
			incoming = preserveMaskedSecrets(incoming, s.sectionLocked("ai_review"),
				[]string{"api_key"})
		}
		next["ai_review"] = normalizeAIReview(incoming)
	}
	if _, ok := next["image_storage"]; ok {
		incoming := next["image_storage"]
		// 保留已存的 WebDAV 密码。
		//
		// 为什么必须做：GET /api/settings 返回的是**脱敏**配置
		// （webdav_password → has_webdav_password）。前端拿不到原文，
		// 回传时该字段是空串 —— 若直接校验，用户「只改了个无关设置」
		// 也会被拦下报「必须填写 WebDAV 密码」，而他早就配好了。
		//
		// 语义与 proxy_runtime 的 _existing_cf_cookies 完全一致：
		// 显式传 has_X=true 且值为空 → 视为「不修改」，沿用磁盘上的值。
		if incomingMap, isMap := incoming.(map[string]any); isMap {
			copied := deepCopyMap(incomingMap)
			if password, _ := copied["webdav_password"].(string); strings.TrimSpace(password) == "" {
				if boolOr(copied["has_webdav_password"], false) {
					if existing := s.imageStoragePasswordLocked(); existing != "" {
						copied["webdav_password"] = existing
					}
				}
			}
			delete(copied, "has_webdav_password")
			incoming = copied
		}
		normalized := normalizeImageStorageSettings(incoming)
		if err := validateImageStorageSettings(normalized); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		next["image_storage"] = normalized
	}
	if _, ok := next["chat_completion_cache"]; ok {
		next["chat_completion_cache"] = normalizeChatCompletionCache(next["chat_completion_cache"])
	}
	if _, ok := next["proxy_runtime"]; ok {
		incoming := next["proxy_runtime"]
		if incomingMap, ok := incoming.(map[string]any); ok {
			previous, _ := s.proxyRuntimeClearanceLocked()
			copied := deepCopyMap(incomingMap)
			copied["_existing_cf_cookies"] = previous["cf_cookies"]
			copied["_existing_cf_clearance"] = previous["cf_clearance"]
			incoming = copied
		}
		next["proxy_runtime"] = normalizeProxyRuntime(incoming)
	}
	delete(next, "backup_state")

	// 先序列化 + 落盘，成功后才更新内存。
	//
	// 回归点：早期写法是 `s.data = next` 在写盘之前。写盘失败时调用方
	// 收到错误，但内存里已经是新值 —— 用户被告知「保存失败」，而后续
	// 任何一次成功写入会把这个值悄悄持久化。
	payload, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	payload = append(payload, '\n')
	writeErr := writeFileAtomic(s.path, payload)
	if writeErr != nil {
		// 落盘失败：内存保持旧值，调用方看到的错误与状态一致。
		s.mu.Unlock()
		return nil, explainConfigWriteError(s.path, writeErr)
	}
	// 只有写盘成功才更新内存 —— 否则「保存失败」后内存里已是新值，
	// 后续任何一次成功写入会把它悄悄持久化。
	s.data = next
	s.mu.Unlock()
	return s.Get(), nil
}

func (s *Store) proxyRuntimeClearanceLocked() (map[string]any, bool) {
	runtime, ok := s.data["proxy_runtime"].(map[string]any)
	if !ok {
		return map[string]any{}, false
	}
	clearance, ok := runtime["clearance"].(map[string]any)
	if !ok {
		return map[string]any{}, false
	}
	return clearance, true
}

// imageStoragePasswordLocked 取磁盘上已存的 WebDAV 密码。调用方必须持 s.mu。
//
// 不能调 raw()：它自己加 RLock，在写锁内调用会死锁（见 LRN-20261027-001）。
func (s *Store) imageStoragePasswordLocked() string {
	settings, ok := s.data["image_storage"].(map[string]any)
	if !ok {
		return ""
	}
	password, _ := settings["webdav_password"].(string)
	return strings.TrimSpace(password)
}

// preserveMaskedSecrets 处理「脱敏字段留空 = 不修改」的通用逻辑。
//
// 背景：GET /api/settings 把敏感字段换成 has_<field> 标记（见 api 层的
// maskConfigSecrets）。前端全量回传所有 section，敏感字段是空串 ——
// 若直接归一化并落盘，用户的密钥就被静默抹掉了（备份连不上、AI 审核失效，
// 且没有任何报错）。这比「校验失败」更难发现。
//
// 规则：字段值为空且 has_<field>=true 时，沿用 previous 里的旧值。
// 事后统一删掉所有 has_* 键，避免它们被持久化。
//
// 调用方必须持 s.mu（previous 直接取自 s.data）。
func preserveMaskedSecrets(incoming map[string]any, previous map[string]any, fields []string) map[string]any {
	out := deepCopyMap(incoming)
	for _, field := range fields {
		value, _ := out[field].(string)
		if strings.TrimSpace(value) != "" {
			continue
		}
		if !boolOr(out["has_"+field], false) {
			continue
		}
		if old, _ := previous[field].(string); strings.TrimSpace(old) != "" {
			out[field] = old
		}
	}
	for key := range out {
		if len(key) > 4 && key[:4] == "has_" {
			delete(out, key)
		}
	}
	return out
}

// sectionLocked 取 s.data 里的某个子对象。调用方必须持 s.mu。
func (s *Store) sectionLocked(key string) map[string]any {
	if section, ok := s.data[key].(map[string]any); ok {
		return section
	}
	return map[string]any{}
}

func validateImageStorageSettings(settings map[string]any) error {
	if !boolOr(settings["enabled"], false) {
		return nil
	}
	url, _ := settings["webdav_url"].(string)
	if strings.TrimSpace(url) == "" {
		return errors.New("启用 WebDAV 图片存储后必须填写 WebDAV URL")
	}
	password, _ := settings["webdav_password"].(string)
	if strings.TrimSpace(password) == "" {
		return errors.New("启用 WebDAV 图片存储后必须填写 WebDAV 密码")
	}
	return nil
}

// explainConfigWriteError 把「写不了 config.json」变成可操作的提示。
//
// 为什么需要：Docker 单文件 bind mount 是最常见的部署形态，而
// config.json 通常是 root:root 644、容器以 uid 65532 运行 ——
// 于是设置页保存失败，用户只看到 "open /app/config.json: permission denied"。
// 这句话没说清是文件权限、目录权限还是挂载问题，也没说怎么修。
//
// 两个失败点都要说明：
//  1. 创建 .tmp 失败 → /app 目录不可写（镜像内目录，属于 root）
//  2. 回退就地写失败 → config.json 文件属主不是 65532
//
// 修复动作在**宿主侧**，容器内改不了 —— 所以提示必须给出宿主命令。
func explainConfigWriteError(path string, err error) error {
	if !isPermissionError(err) {
		return err
	}
	return fmt.Errorf(
		"写入 %s 失败：%w\n"+
			"这是 Docker 部署的常见问题 —— config.json 以单文件 bind mount 挂进容器，\n"+
			"而容器以非 root（uid 65532）运行。请在**宿主机**上执行：\n"+
			"  sudo chown 65532:65532 %s\n"+
			"或让容器以 root 运行（compose 里加 user: \"0:0\"，不推荐）。\n"+
			"也可改用环境变量覆盖需要改的项，避免写文件。",
		path, err, hostHintPath(path))
}

// hostHintPath 把容器内路径换回宿主侧相对路径，让提示可直接复制执行。
// /app/config.json → config.json（compose 里 source 就是 ./config.json）
func hostHintPath(path string) string {
	cleaned := filepath.ToSlash(path)
	for _, prefix := range []string{"/app/", "/data/"} {
		if strings.HasPrefix(cleaned, prefix) {
			return strings.TrimPrefix(cleaned, prefix)
		}
	}
	return cleaned
}

// isPermissionError 判断错误链里是否有 permission denied。
func isPermissionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "permission denied")
}

func writeFileAtomic(path string, payload []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		// Docker 单文件 bind mount 会拒绝 rename（EBUSY），退回就地写。
		return os.WriteFile(path, payload, 0o600)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return os.WriteFile(path, payload, 0o600)
	}
	return nil
}

// ---- 基础类型归一化工具 ----

func boolOr(value any, fallback bool) bool {
	switch typed := value.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
		return fallback
	case nil:
		return fallback
	case bool:
		return typed
	case float64:
		return typed != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	default:
		return fallback
	}
}

func intOr(value any, fallback int) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return int(parsed), true
		}
		return fallback, false
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return parsed, true
		}
		return fallback, false
	case bool:
		return fallback, false
	default:
		return fallback, false
	}
}

func positiveIntOr(value any, fallback, minimum int) int {
	parsed, ok := intOr(value, fallback)
	if !ok {
		parsed = fallback
	}
	if parsed < minimum {
		return minimum
	}
	return parsed
}

func floatOr(value any, fallback float64) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return parsed
		}
		return fallback
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
			return parsed
		}
		return fallback
	default:
		return fallback
	}
}

func maxFloat(value, floor float64) float64 {
	if value < floor {
		return floor
	}
	return value
}

func deepCopyMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = deepCopyJSONValue(value)
	}
	return out
}
