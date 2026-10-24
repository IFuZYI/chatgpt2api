package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 敏感词命中必须拒绝，且不发任何网络请求。
//
// 回归点：Go 侧完全没有 content filter —— SensitiveWords() 与 AIReview()
// 只被 /api/settings 回显，请求链路零调用。管理员配了敏感词也不拦截。
// Python: services/content_filter.py:150-157。
func TestCheckRequestRejectsSensitiveWord(t *testing.T) {
	filter := NewContentFilter(ContentFilterOptions{
		SensitiveWords: func() []string { return []string{"违禁词"} },
		AIReview:       func() map[string]any { return map[string]any{"enabled": false} },
	})

	err := filter.Check(context.Background(), "这是一个违禁词的请求")
	if err == nil {
		t.Fatal("sensitive word not rejected")
	}
	if err.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", err.StatusCode)
	}
	if !strings.Contains(err.Message, "敏感词") {
		t.Errorf("message = %q, want it to mention 敏感词", err.Message)
	}
}

// 敏感词未命中时放行，且 AI 审核未启用时完全不发网络请求。
func TestCheckRequestPassesWithoutSensitiveWord(t *testing.T) {
	probed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probed = true
	}))
	defer server.Close()

	filter := NewContentFilter(ContentFilterOptions{
		SensitiveWords: func() []string { return []string{"违禁词"} },
		AIReview: func() map[string]any {
			return map[string]any{"enabled": false, "base_url": server.URL}
		},
	})

	if err := filter.Check(context.Background(), "正常请求"); err != nil {
		t.Fatalf("normal request rejected: %v", err)
	}
	if probed {
		t.Error("AI review disabled but a network request was made")
	}
}

// AI 审核返回 REJECT 必须拒绝。
func TestCheckRequestRejectsOnAIReject(t *testing.T) {
	server := reviewServer(t, "REJECT")
	defer server.Close()

	filter := NewContentFilter(ContentFilterOptions{
		SensitiveWords: func() []string { return nil },
		AIReview: func() map[string]any {
			return map[string]any{
				"enabled":  true,
				"base_url": server.URL,
				"api_key":  "k",
				"model":    "m",
			}
		},
	})

	err := filter.Check(context.Background(), "某个请求")
	if err == nil {
		t.Fatal("AI reject not enforced")
	}
	if !strings.Contains(err.Message, "AI 审核") {
		t.Errorf("message = %q, want it to mention AI 审核", err.Message)
	}
}

// AI 审核返回 ALLOW 必须放行。
func TestCheckRequestAllowsOnAIAllow(t *testing.T) {
	server := reviewServer(t, "ALLOW")
	defer server.Close()

	filter := NewContentFilter(ContentFilterOptions{
		SensitiveWords: func() []string { return nil },
		AIReview: func() map[string]any {
			return map[string]any{
				"enabled":  true,
				"base_url": server.URL,
				"api_key":  "k",
				"model":    "m",
			}
		},
	})

	if err := filter.Check(context.Background(), "某个请求"); err != nil {
		t.Fatalf("AI allow rejected: %v", err)
	}
}

// 配置不完整必须报错（Python: ai review config is incomplete）。
func TestCheckRequestRejectsIncompleteReviewConfig(t *testing.T) {
	filter := NewContentFilter(ContentFilterOptions{
		SensitiveWords: func() []string { return nil },
		AIReview: func() map[string]any {
			return map[string]any{"enabled": true, "base_url": "", "api_key": "", "model": ""}
		},
	})

	err := filter.Check(context.Background(), "请求")
	if err == nil {
		t.Fatal("incomplete config not reported")
	}
	if !strings.Contains(err.Message, "incomplete") {
		t.Errorf("message = %q, want it to mention incomplete", err.Message)
	}
}

// fail_open=true（默认）时审核服务故障放行；fail_open=false 时拒绝。
func TestCheckRequestFailOpenPolicy(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	openFilter := NewContentFilter(ContentFilterOptions{
		SensitiveWords: func() []string { return nil },
		AIReview: func() map[string]any {
			return map[string]any{"enabled": true, "base_url": broken.URL, "api_key": "k", "model": "m", "fail_open": true}
		},
	})
	if err := openFilter.Check(context.Background(), "请求"); err != nil {
		t.Errorf("fail_open=true should let the request through, got %v", err)
	}

	closedFilter := NewContentFilter(ContentFilterOptions{
		SensitiveWords: func() []string { return nil },
		AIReview: func() map[string]any {
			return map[string]any{"enabled": true, "base_url": broken.URL, "api_key": "k", "model": "m", "fail_open": false}
		},
	})
	if err := closedFilter.Check(context.Background(), "请求"); err == nil {
		t.Error("fail_open=false should reject when the review service is down")
	}
}

// base64 图片必须在送审前剥离（否则一次内联图片就撑爆审核模型的上下文）。
func TestSanitizeForReviewStripsBase64(t *testing.T) {
	input := "看图 data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg== 结束"

	sanitized, stats := sanitizeForReview(input)

	if strings.Contains(sanitized, "base64") {
		t.Errorf("base64 not stripped: %q", sanitized)
	}
	if !strings.Contains(sanitized, "[image]") {
		t.Errorf("missing [image] placeholder: %q", sanitized)
	}
	if stats.Base64BlocksStripped != 1 {
		t.Errorf("stripped = %d, want 1", stats.Base64BlocksStripped)
	}
}

// reviewServer 起一个返回指定决策的 OpenAI 兼容审核服务。
func reviewServer(t *testing.T, decision string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("review probed %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q, want Bearer k", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{
				map[string]any{"message": map[string]any{"content": decision}},
			},
		})
	}))
}
