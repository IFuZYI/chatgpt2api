package protocol

import (
	"sync"
	"testing"
	"time"
)

// 期望值由 Python 的 cache_key 实测产出（同一 body/messages）。
func TestCacheKeyMatchesPython(t *testing.T) {
	body := map[string]any{
		"model":       "gpt-5",
		"temperature": 0.7,
		"extra":       "ignored", // 不在可缓存字段里，必须被忽略
	}
	messages := []map[string]any{{"role": "user", "content": "hi"}}

	if got := CacheKey(body, messages, false); got != "698eb4db7cc79ec7542677c34cf6ec5459141c51c6ac3bd5b7dbbec07750ce4e" {
		t.Errorf("non-stream key = %s", got)
	}
	if got := CacheKey(body, messages, true); got != "ba50f753bf59fbe9cdc667ae9f7ae210259d22b3a32b97e3caab513063455afd" {
		t.Errorf("stream key = %s", got)
	}
}

// 不可缓存字段变化不应改变 key。
func TestCacheKeyIgnoresUncacheableFields(t *testing.T) {
	messages := []map[string]any{{"role": "user", "content": "hi"}}
	base := map[string]any{"model": "gpt-5"}
	withExtra := map[string]any{"model": "gpt-5", "n": 4, "prompt": "ignored", "api_key": "x"}

	if CacheKey(base, messages, false) != CacheKey(withExtra, messages, false) {
		t.Error("uncacheable fields must not affect cache key")
	}
}

func TestNormalizeTextMessages(t *testing.T) {
	settings := CacheSettings{
		NormalizeMessages:      true,
		DropAdjacentDuplicates: true,
		DropAssistantHistory:   false,
	}
	input := []map[string]any{
		{"role": "user", "content": "a"},
		{"role": "user", "content": "a"}, // 相邻重复，应删
		{"role": "assistant", "content": "b"},
	}
	got := NormalizeTextMessages(input, settings)
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2: %v", len(got), got)
	}
	if stringOf(got[0]["content"]) != "a" || stringOf(got[1]["content"]) != "b" {
		t.Errorf("wrong messages: %v", got)
	}
}

func TestNormalizeTextMessagesDropAssistantHistory(t *testing.T) {
	settings := CacheSettings{
		NormalizeMessages:    true,
		DropAssistantHistory: true,
	}
	input := []map[string]any{
		{"role": "user", "content": "a"},
		{"role": "assistant", "content": "b"},
		{"role": "user", "content": "c"},
	}
	got := NormalizeTextMessages(input, settings)
	if len(got) != 2 {
		t.Fatalf("assistant history should be dropped, got %v", got)
	}
	for _, message := range got {
		if stringOf(message["role"]) == "assistant" {
			t.Errorf("assistant message survived: %v", message)
		}
	}
}

// 关掉 normalize 时原样返回。
func TestNormalizeTextMessagesDisabled(t *testing.T) {
	settings := CacheSettings{NormalizeMessages: false}
	input := []map[string]any{
		{"role": "user", "content": "a"},
		{"role": "user", "content": "a"},
	}
	got := NormalizeTextMessages(input, settings)
	if len(got) != 2 {
		t.Fatalf("normalize disabled should keep all messages, got %d", len(got))
	}
}

func enabledSettings() CacheSettings {
	return CacheSettings{
		Enabled:                true,
		TTLSeconds:             60,
		MaxEntries:             16,
		DedupeInflight:         true,
		StreamCache:            true,
		NormalizeMessages:      true,
		DropAdjacentDuplicates: true,
	}
}

func TestCacheHit(t *testing.T) {
	cache := NewChatCompletionCache(enabledSettings)
	calls := 0
	compute := func() (map[string]any, error) {
		calls++
		return map[string]any{"value": calls}, nil
	}

	first, err := cache.GetOrComputeResponse("k", compute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.GetOrComputeResponse("k", compute)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("compute called %d times, want 1", calls)
	}
	if first["value"] != second["value"] {
		t.Error("cached value should equal first result")
	}
}

// 返回的是深拷贝，调用方修改不应污染缓存。
func TestCacheReturnsDeepCopy(t *testing.T) {
	cache := NewChatCompletionCache(enabledSettings)
	compute := func() (map[string]any, error) {
		return map[string]any{"nested": map[string]any{"x": "original"}}, nil
	}
	first, _ := cache.GetOrComputeResponse("k", compute)
	nested, _ := first["nested"].(map[string]any)
	nested["x"] = "mutated"

	second, _ := cache.GetOrComputeResponse("k", compute)
	nested2, _ := second["nested"].(map[string]any)
	if nested2["x"] != "original" {
		t.Fatalf("cache was polluted: %v", nested2["x"])
	}
}

func TestCacheDisabled(t *testing.T) {
	settings := enabledSettings()
	settings.Enabled = false
	cache := NewChatCompletionCache(func() CacheSettings { return settings })

	calls := 0
	compute := func() (map[string]any, error) {
		calls++
		return map[string]any{}, nil
	}
	cache.GetOrComputeResponse("k", compute)
	cache.GetOrComputeResponse("k", compute)
	if calls != 2 {
		t.Fatalf("disabled cache should always compute, got %d calls", calls)
	}
}

func TestCacheExpires(t *testing.T) {
	settings := enabledSettings()
	settings.TTLSeconds = 1
	cache := NewChatCompletionCache(func() CacheSettings { return settings })

	calls := 0
	compute := func() (map[string]any, error) {
		calls++
		return map[string]any{}, nil
	}
	cache.GetOrComputeResponse("k", compute)
	time.Sleep(1100 * time.Millisecond)
	cache.GetOrComputeResponse("k", compute)
	if calls != 2 {
		t.Fatalf("expired entry should recompute, got %d calls", calls)
	}
}

// 并发同 key 只算一次（in-flight 去重）。
func TestCacheDedupesInflight(t *testing.T) {
	cache := NewChatCompletionCache(enabledSettings)
	var calls int32
	var mu sync.Mutex
	compute := func() (map[string]any, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return map[string]any{"ok": true}, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cache.GetOrComputeResponse("k", compute); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("inflight dedupe failed: compute called %d times", calls)
	}
}

func TestCacheMaxEntries(t *testing.T) {
	settings := enabledSettings()
	settings.MaxEntries = 2
	cache := NewChatCompletionCache(func() CacheSettings { return settings })

	for _, key := range []string{"a", "b", "c"} {
		cache.GetOrComputeResponse(key, func() (map[string]any, error) {
			return map[string]any{}, nil
		})
	}
	cache.mu.Lock()
	size := len(cache.entries)
	cache.mu.Unlock()
	if size > 2 {
		t.Fatalf("cache size = %d, want <= 2", size)
	}
}

func TestCacheStream(t *testing.T) {
	cache := NewChatCompletionCache(enabledSettings)
	calls := 0
	compute := func() ([]map[string]any, error) {
		calls++
		return []map[string]any{{"delta": "x"}}, nil
	}
	cache.GetOrComputeStream("k", compute)
	cache.GetOrComputeStream("k", compute)
	if calls != 1 {
		t.Fatalf("stream cache should dedupe, got %d calls", calls)
	}
}

// stream_cache 关闭时不缓存流式结果。
func TestCacheStreamDisabled(t *testing.T) {
	settings := enabledSettings()
	settings.StreamCache = false
	cache := NewChatCompletionCache(func() CacheSettings { return settings })

	calls := 0
	compute := func() ([]map[string]any, error) {
		calls++
		return nil, nil
	}
	cache.GetOrComputeStream("k", compute)
	cache.GetOrComputeStream("k", compute)
	if calls != 2 {
		t.Fatalf("stream cache disabled should always compute, got %d", calls)
	}
}
