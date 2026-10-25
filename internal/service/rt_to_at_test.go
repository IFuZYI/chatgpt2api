package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// 用 refresh_token 换 access_token：验证请求形状与 Python 一致
// （form-urlencoded，grant_type=refresh_token，client_id 必带）。
func TestRefreshAccessTokenRequestShape(t *testing.T) {
	var gotForm url.Values
	var gotCT string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(raw))
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-rotated","id_token":"id-new"}`))
	}))
	defer server.Close()

	tokens, err := RefreshAccessTokenVia(context.Background(), server.URL, "rt-1", nil)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if gotForm.Get("grant_type") != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", gotForm.Get("grant_type"))
	}
	if gotForm.Get("refresh_token") != "rt-1" {
		t.Errorf("refresh_token = %q, want rt-1", gotForm.Get("refresh_token"))
	}
	if gotForm.Get("client_id") != oauthClientIDConst {
		t.Errorf("client_id = %q, want %q", gotForm.Get("client_id"), oauthClientIDConst)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want form-urlencoded（不是 JSON）", gotCT)
	}
	if tokens["access_token"] != "at-new" || tokens["refresh_token"] != "rt-rotated" {
		t.Errorf("tokens = %v", tokens)
	}
}

// 上游没返回新 refresh_token 时必须沿用旧的（Python 同样回退）。
func TestRefreshAccessTokenKeepsOldRefreshTokenWhenAbsent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at-new"}`))
	}))
	defer server.Close()

	tokens, err := RefreshAccessTokenVia(context.Background(), server.URL, "rt-old", nil)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tokens["refresh_token"] != "rt-old" {
		t.Errorf("refresh_token = %q，应回退到旧的 rt-old", tokens["refresh_token"])
	}
}

// 非 200 或没有 access_token 时必须报错，不能返回空 token 让调用方以为成功。
func TestRefreshAccessTokenRejectsFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"401 带 error", 401, `{"error":"invalid_grant","error_description":"token expired"}`},
		{"200 但没有 access_token", 200, `{"token_type":"Bearer"}`},
		{"500 纯文本", 500, `boom`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			if _, err := RefreshAccessTokenVia(context.Background(), server.URL, "rt", nil); err == nil {
				t.Fatalf("status=%d body=%s 应报错", tc.status, tc.body)
			}
		})
	}
}

// 错误信息要带状态码与上游 detail，便于判断是 rt 失效还是被限流。
//
// 只断言 error_description（Python 也是 `error_description or error` 的优先级），
// 不要求 error 码也出现 —— 那是上游的次要字段。
func TestRefreshAccessTokenErrorMentionsStatusAndDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "invalid_grant", "error_description": "refresh token revoked",
		})
	}))
	defer server.Close()

	_, err := RefreshAccessTokenVia(context.Background(), server.URL, "rt", nil)
	if err == nil {
		t.Fatal("应报错")
	}
	msg := err.Error()
	for _, want := range []string{"401", "refresh token revoked"} {
		if !containsSubstring(msg, want) {
			t.Errorf("错误信息 %q 缺少 %q", msg, want)
		}
	}
}

func containsSubstring(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
