package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

// CacheableTextKeys 是参与缓存键的请求字段（逐字取自 chat_completion_cache.py）。
var CacheableTextKeys = []string{
	"frequency_penalty",
	"max_completion_tokens",
	"max_tokens",
	"metadata",
	"model",
	"presence_penalty",
	"reasoning_effort",
	"response_format",
	"seed",
	"stop",
	"temperature",
	"thinking_effort",
	"tool_choice",
	"tools",
	"top_p",
	"user",
	"reasoning",
}

// CacheSettings 是缓存配置的运行时快照。
type CacheSettings struct {
	Enabled                bool
	TTLSeconds             int
	MaxEntries             int
	DedupeInflight         bool
	StreamCache            bool
	NormalizeMessages      bool
	DropAdjacentDuplicates bool
	DropAssistantHistory   bool
}

// CacheEntry 是一条缓存。
type CacheEntry struct {
	expiresAt time.Time
	value     any
}

// InflightCall 是「同一请求正在算」的占位，用于合并并发重复请求。
type InflightCall struct {
	done  chan struct{}
	value any
	err   error
}

// ChatCompletionCache 复刻 services/protocol/chat_completion_cache.py。
//
// 三个能力：
//   - 短 TTL 缓存（默认 60s）
//   - in-flight 去重（同 key 并发只算一次，其余等结果）
//   - 消息规范化（去相邻重复 / 可选去 assistant 历史）
type ChatCompletionCache struct {
	settings func() CacheSettings

	mu       sync.Mutex
	entries  map[string]*CacheEntry
	inflight map[string]*InflightCall
}

// NewChatCompletionCache 构造。settings 每次调用都重新读配置，
// 所以设置页改完立即生效（与 Python 一致）。
func NewChatCompletionCache(settings func() CacheSettings) *ChatCompletionCache {
	return &ChatCompletionCache{
		settings: settings,
		entries:  map[string]*CacheEntry{},
		inflight: map[string]*InflightCall{},
	}
}

// Clear 清空缓存与在途表。
func (c *ChatCompletionCache) Clear() {
	c.mu.Lock()
	c.entries = map[string]*CacheEntry{}
	c.inflight = map[string]*InflightCall{}
	c.mu.Unlock()
}

// pruneLocked 删除过期条目，并按过期时间淘汰到 maxEntries 以内。
func (c *ChatCompletionCache) pruneLocked(now time.Time, maxEntries int) {
	for key, entry := range c.entries {
		if !entry.expiresAt.After(now) {
			delete(c.entries, key)
		}
	}
	for len(c.entries) > maxEntries {
		oldestKey := ""
		var oldest time.Time
		for key, entry := range c.entries {
			if oldestKey == "" || entry.expiresAt.Before(oldest) {
				oldestKey = key
				oldest = entry.expiresAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(c.entries, oldestKey)
	}
}

// jsonSafe 把不可 JSON 化的值（bytes）转成可比较的摘要。
func jsonSafe(value any) any {
	switch typed := value.(type) {
	case []byte:
		sum := sha256.Sum256(typed)
		return map[string]any{
			"__bytes_sha256__": hex.EncodeToString(sum[:]),
			"length":           len(typed),
		}
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = jsonSafe(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = jsonSafe(item)
		}
		return out
	default:
		return value
	}
}

// CanonicalBody 复刻 canonical_body：只取可缓存字段 + messages + stream。
func CanonicalBody(body map[string]any, messages []map[string]any, stream bool) map[string]any {
	payload := map[string]any{}
	for _, key := range CacheableTextKeys {
		if value, ok := body[key]; ok {
			payload[key] = value
		}
	}
	payload["messages"] = messages
	payload["stream"] = stream
	return payload
}

// CacheKey 复刻 cache_key：规范化 JSON 后取 sha256。
func CacheKey(body map[string]any, messages []map[string]any, stream bool) string {
	canonical := CanonicalBody(body, messages, stream)
	safe := jsonSafe(canonical)
	encoded, err := canonicalJSON(safe)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// canonicalJSON 输出键名排序、无空格的 JSON（与 Python 的 sort_keys + separators 一致）。
func canonicalJSON(value any) ([]byte, error) {
	// Go 的 json.Marshal 对 map 已经按键名排序，且不输出多余空格。
	return json.Marshal(value)
}

// messageSignature 用于相邻去重比较。
func messageSignature(message map[string]any) string {
	encoded, err := json.Marshal(jsonSafe(message))
	if err != nil {
		return ""
	}
	return string(encoded)
}

// NormalizeTextMessages 复刻 normalize_text_messages。
func NormalizeTextMessages(messages []map[string]any, settings CacheSettings) []map[string]any {
	if !settings.NormalizeMessages {
		return messages
	}
	out := make([]map[string]any, 0, len(messages))
	previous := ""
	for _, message := range messages {
		if settings.DropAssistantHistory && strings.TrimSpace(stringOf(message["role"])) == "assistant" {
			continue
		}
		signature := messageSignature(message)
		if settings.DropAdjacentDuplicates && signature == previous {
			continue
		}
		out = append(out, message)
		previous = signature
	}
	return out
}

// deepCopyValue 深拷贝缓存值，避免调用方修改污染缓存。
func deepCopyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = deepCopyValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = deepCopyValue(item)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(typed))
		for index, item := range typed {
			out[index] = deepCopyValue(item).(map[string]any)
		}
		return out
	default:
		return value
	}
}

// GetOrComputeResponse 复刻 get_or_compute_response（非流式）。
func (c *ChatCompletionCache) GetOrComputeResponse(key string, compute func() (map[string]any, error)) (map[string]any, error) {
	settings := c.settings()
	if !settings.Enabled || settings.TTLSeconds <= 0 || key == "" {
		return compute()
	}

	now := time.Now()
	maxEntries := settings.MaxEntries
	if maxEntries < 1 {
		maxEntries = 1
	}

	c.mu.Lock()
	c.pruneLocked(now, maxEntries)
	if entry, ok := c.entries[key]; ok && entry.expiresAt.After(now) {
		value, _ := deepCopyValue(entry.value).(map[string]any)
		c.mu.Unlock()
		return value, nil
	}
	var call *InflightCall
	owner := false
	if settings.DedupeInflight {
		if existing, ok := c.inflight[key]; ok {
			call = existing
		} else {
			call = &InflightCall{done: make(chan struct{})}
			c.inflight[key] = call
			owner = true
		}
	}
	c.mu.Unlock()

	if !owner && call != nil {
		<-call.done
		if call.err != nil {
			return nil, call.err
		}
		value, _ := deepCopyValue(call.value).(map[string]any)
		return value, nil
	}

	value, err := compute()

	if call != nil {
		c.mu.Lock()
		call.value = value
		call.err = err
		delete(c.inflight, key)
		c.mu.Unlock()
		close(call.done)
	}

	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[key] = &CacheEntry{
		expiresAt: now.Add(time.Duration(settings.TTLSeconds) * time.Second),
		value:     deepCopyValue(value),
	}
	// 写入后再裁一次：Python 的 _prune_locked 在**读取时**执行，
	// 但它同时限制了容量，所以写入后必须收敛到 maxEntries 以内。
	c.pruneLocked(now, maxEntries)
	c.mu.Unlock()
	return value, nil
}

// GetOrComputeStream 复刻 get_or_compute_stream（流式）。
//
// 语义：先完整算完再回放。所以「流式缓存」并不会真的流式产出——
// 它把整段结果存起来，后续相同请求瞬间回放。这是 Python 的既有行为。
func (c *ChatCompletionCache) GetOrComputeStream(key string, compute func() ([]map[string]any, error)) ([]map[string]any, error) {
	settings := c.settings()
	if !settings.Enabled || !settings.StreamCache || settings.TTLSeconds <= 0 || key == "" {
		return compute()
	}

	now := time.Now()
	maxEntries := settings.MaxEntries
	if maxEntries < 1 {
		maxEntries = 1
	}

	c.mu.Lock()
	c.pruneLocked(now, maxEntries)
	if entry, ok := c.entries[key]; ok && entry.expiresAt.After(now) {
		value, _ := deepCopyValue(entry.value).([]map[string]any)
		c.mu.Unlock()
		return value, nil
	}
	var call *InflightCall
	owner := false
	if settings.DedupeInflight {
		if existing, ok := c.inflight[key]; ok {
			call = existing
		} else {
			call = &InflightCall{done: make(chan struct{})}
			c.inflight[key] = call
			owner = true
		}
	}
	c.mu.Unlock()

	if !owner && call != nil {
		<-call.done
		if call.err != nil {
			return nil, call.err
		}
		value, _ := deepCopyValue(call.value).([]map[string]any)
		return value, nil
	}

	value, err := compute()

	if call != nil {
		c.mu.Lock()
		call.value = value
		call.err = err
		delete(c.inflight, key)
		c.mu.Unlock()
		close(call.done)
	}

	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[key] = &CacheEntry{
		expiresAt: now.Add(time.Duration(settings.TTLSeconds) * time.Second),
		value:     deepCopyValue(value),
	}
	c.pruneLocked(now, maxEntries)
	c.mu.Unlock()
	return value, nil
}

// SortCacheableKeys 返回排序后的可缓存字段名（测试与调试用）。
func SortCacheableKeys() []string {
	out := append([]string{}, CacheableTextKeys...)
	sort.Strings(out)
	return out
}
