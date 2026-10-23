package config

import (
	"strings"
)

// normalizeBackupSettings 复刻 Python 的 _normalize_backup_settings。
func normalizeBackupSettings(value any) map[string]any {
	source, _ := value.(map[string]any)
	if source == nil {
		source = map[string]any{}
	}
	return map[string]any{
		"enabled":           boolOr(source["enabled"], false),
		"provider":          "cloudflare_r2",
		"account_id":        strOr(source["account_id"], ""),
		"access_key_id":     strOr(source["access_key_id"], ""),
		"secret_access_key": strOr(source["secret_access_key"], ""),
		"bucket":            strOr(source["bucket"], ""),
		"prefix":            trimSlashesOr(strOr(source["prefix"], ""), "backups"),
		"interval_minutes":  positiveIntOr(source["interval_minutes"], 360, 1),
		"rotation_keep":     positiveIntOr(source["rotation_keep"], 10, 0),
		"encrypt":           boolOr(source["encrypt"], false),
		"passphrase":        strOr(source["passphrase"], ""),
		"include":           normalizeBackupInclude(source["include"]),
	}
}

func normalizeBackupInclude(value any) map[string]any {
	source, _ := value.(map[string]any)
	out := make(map[string]any, len(DefaultBackupInclude))
	for key, fallback := range DefaultBackupInclude {
		out[key] = boolOr(source[key], fallback)
	}
	return out
}

// normalizeImageStorageSettings 复刻 Python 的 _normalize_image_storage_settings。
func normalizeImageStorageSettings(value any) map[string]any {
	source, _ := value.(map[string]any)
	if source == nil {
		source = map[string]any{}
	}
	mode := strings.ToLower(strings.TrimSpace(strOr(source["mode"], "local")))
	if mode == "" {
		mode = "local"
	}
	switch mode {
	case "local", "webdav", "both":
	default:
		mode = "local"
	}
	enabled := boolOr(source["enabled"], false)
	if !enabled {
		mode = "local"
	}
	rootPath := strings.Trim(strOr(source["webdav_root_path"], "chatgpt2api/images"), "/")
	rootPath = strings.TrimSpace(rootPath)
	if rootPath == "" {
		rootPath = "chatgpt2api/images"
	}
	return map[string]any{
		"enabled":          enabled,
		"mode":             mode,
		"webdav_url":       strings.TrimRight(strings.TrimSpace(strOr(source["webdav_url"], "")), "/"),
		"webdav_username":  strings.TrimSpace(strOr(source["webdav_username"], "")),
		"webdav_password":  strings.TrimSpace(strOr(source["webdav_password"], "")),
		"webdav_root_path": rootPath,
		"public_base_url":  strings.TrimRight(strings.TrimSpace(strOr(source["public_base_url"], "")), "/"),
	}
}

// normalizeChatCompletionCache 复刻 Python 的 _normalize_chat_completion_cache_settings。
func normalizeChatCompletionCache(value any) map[string]any {
	source, _ := value.(map[string]any)
	if source == nil {
		source = map[string]any{}
	}
	return map[string]any{
		"enabled": boolOr(source["enabled"], true),
		"ttl_seconds": positiveIntOr(
			source["ttl_seconds"], 60, 0,
		),
		"max_entries": positiveIntOr(source["max_entries"], 256, 1),
		"dedupe_inflight": boolOr(
			source["dedupe_inflight"], true,
		),
		"stream_cache": boolOr(source["stream_cache"], true),
		"normalize_messages": boolOr(
			source["normalize_messages"], true,
		),
		"drop_adjacent_duplicates": boolOr(
			source["drop_adjacent_duplicates"], true,
		),
		"drop_assistant_history": boolOr(
			source["drop_assistant_history"], false,
		),
	}
}

// normalizeProxyRuntime 复刻 Python 的 _normalize_proxy_runtime_settings。
// 注意 _existing_cf_cookies / _existing_cf_clearance 是内部传递字段，
// 用于「前端回传空值时保留原 cookie」。
func normalizeProxyRuntime(value any) map[string]any {
	source, _ := value.(map[string]any)
	if source == nil {
		source = map[string]any{}
	}
	defaultClearance, _ := DefaultProxyRuntime["clearance"].(map[string]any)
	clearanceSource, _ := source["clearance"].(map[string]any)
	if clearanceSource == nil {
		clearanceSource = map[string]any{}
	}

	egressMode := strings.ToLower(strings.TrimSpace(strOr(source["egress_mode"], "direct")))
	if egressMode == "" {
		egressMode = "direct"
	}
	switch egressMode {
	case "direct", "single_proxy":
	default:
		egressMode = "direct"
	}

	clearanceMode := strings.ToLower(strings.TrimSpace(strOr(clearanceSource["mode"], "none")))
	if clearanceMode == "" {
		clearanceMode = "none"
	}
	switch clearanceMode {
	case "none", "manual", "flaresolverr":
	default:
		clearanceMode = "none"
	}

	userAgent := strings.TrimSpace(strOr(clearanceSource["user_agent"], ""))
	if userAgent == "" {
		userAgent = strOr(defaultClearance["user_agent"], DefaultProxyRuntimeUserAgent)
	}
	browser := strings.TrimSpace(strOr(clearanceSource["browser"], ""))
	if browser == "" {
		browser = strOr(defaultClearance["browser"], "chrome")
	}

	existingCookies := strings.TrimSpace(strOr(source["_existing_cf_cookies"], ""))
	existingClearance := strings.TrimSpace(strOr(source["_existing_cf_clearance"], ""))
	cfCookies := strings.TrimSpace(strOr(clearanceSource["cf_cookies"], ""))
	cfClearance := strings.TrimSpace(strOr(clearanceSource["cf_clearance"], ""))
	if cfCookies == "" && boolOr(clearanceSource["has_cf_cookies"], false) {
		cfCookies = existingCookies
	}
	if cfClearance == "" && boolOr(clearanceSource["has_cf_clearance"], false) {
		cfClearance = existingClearance
	}

	return map[string]any{
		"enabled":                    boolOr(source["enabled"], false),
		"egress_mode":                egressMode,
		"proxy_url":                  strings.TrimSpace(strOr(source["proxy_url"], "")),
		"resource_proxy_url":         strings.TrimSpace(strOr(source["resource_proxy_url"], "")),
		"skip_ssl_verify":            boolOr(source["skip_ssl_verify"], false),
		"reset_session_status_codes": normalizeStatusCodes(source["reset_session_status_codes"]),
		"clearance": map[string]any{
			"enabled":          boolOr(clearanceSource["enabled"], false),
			"mode":             clearanceMode,
			"cf_cookies":       cfCookies,
			"cf_clearance":     cfClearance,
			"user_agent":       userAgent,
			"browser":          browser,
			"flaresolverr_url": strings.TrimSpace(strOr(clearanceSource["flaresolverr_url"], "")),
			"timeout_sec":      positiveIntOr(clearanceSource["timeout_sec"], 60, 1),
			"refresh_interval": positiveIntOr(clearanceSource["refresh_interval"], 3600, 60),
			"warm_up_on_start": boolOr(clearanceSource["warm_up_on_start"], false),
		},
	}
}

func normalizeStatusCodes(value any) []any {
	items, ok := value.([]any)
	if !ok {
		return []any{403}
	}
	out := make([]any, 0, len(items))
	seen := map[int]bool{}
	for _, item := range items {
		if _, isBool := item.(bool); isBool {
			continue
		}
		status, ok := intOr(item, 0)
		if !ok {
			continue
		}
		if status < 100 || status > 599 || seen[status] {
			continue
		}
		seen[status] = true
		out = append(out, status)
	}
	if len(out) == 0 {
		return []any{403}
	}
	return out
}

func strOr(value any, fallback string) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fallback
}

func trimOr(value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	return trimmed
}

// trimSlashesOr 复刻 Python 的 `str(...).strip().strip("/") or fallback`。
func trimSlashesOr(value, fallback string) string {
	trimmed := strings.Trim(strings.TrimSpace(value), "/")
	if trimmed == "" {
		return fallback
	}
	return trimmed
}
