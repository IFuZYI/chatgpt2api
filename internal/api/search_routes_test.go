package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/api"
	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/IFuZYI/chatgpt2api/internal/service"
)

// fakeSearchUpstream 是 protocol.SearchUpstream 的测试替身：
// 记录实参并回放结果，完全不碰网络。
type fakeSearchUpstream struct {
	tokens  []string
	prompts []string
	models  []string
	result  map[string]any
	err     error
}

func (f *fakeSearchUpstream) Search(ctx context.Context, prompt, model string) (map[string]any, error) {
	f.prompts = append(f.prompts, prompt)
	f.models = append(f.models, model)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

// searchResultFixture 是一份典型的搜索结果（字段与 Python _extract_search_result 一致）。
func searchResultFixture() map[string]any {
	return map[string]any{
		"conversation_id":      "cid-1",
		"status":               "finished_successfully",
		"answer":               "答案是 chatgpt2api",
		"sources":              []any{map[string]any{"title": "GitHub", "url": "https://github.com/IFuZYI/chatgpt2api", "snippet": "", "source_type": "web"}},
		"assistant_message_id": "m-1",
		"create_time":          12.5,
	}
}

// newSearchRouteServer 建一个注入了假搜索上游的测试服务器。
func newSearchRouteServer(
	t *testing.T,
	accounts []map[string]any,
	upstream *fakeSearchUpstream,
) (*httptest.Server, *api.Deps) {
	t.Helper()
	deps := newTestDeps(t)
	if len(accounts) > 0 {
		deps.Accts.AddAccountItems(accounts, nil)
	}
	deps.SearchUpstream = func(ctx context.Context, accessToken string) (protocol.SearchUpstream, func(), error) {
		upstream.tokens = append(upstream.tokens, accessToken)
		return upstream, func() {}, nil
	}
	server := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(server.Close)
	return server, deps
}

// 正常路径：取文本号 → 上游搜索 → 原样返回结果 + _account_email → 记账。
func TestSearchRouteReturnsResultAndMarksTextUsed(t *testing.T) {
	upstream := &fakeSearchUpstream{result: searchResultFixture()}
	server, deps := newSearchRouteServer(t, []map[string]any{
		{"access_token": "search-token", "email": "a@b.c", "type": "plus", "status": service.StatusNormal},
	}, upstream)

	// model 是多余字段：Python 的 SearchRequest 只收 prompt，模型固定 gpt-5-5。
	status, payload := postJSON(t, server, "/v1/search", map[string]any{
		"prompt": "帮我搜索 chatgpt2api",
		"model":  "whatever",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, payload)
	}
	if body["answer"] != "答案是 chatgpt2api" || body["status"] != "finished_successfully" {
		t.Errorf("body = %s", payload)
	}
	if body["conversation_id"] != "cid-1" || body["assistant_message_id"] != "m-1" {
		t.Errorf("body = %s", payload)
	}
	if body["create_time"] != 12.5 {
		t.Errorf("create_time = %#v", body["create_time"])
	}
	sources, ok := body["sources"].([]any)
	if !ok || len(sources) != 1 {
		t.Fatalf("sources = %#v", body["sources"])
	}
	// 账号邮箱只进 call 日志，不进响应。
	// Python 的 handle() 会塞 _account_email，但 LoggedCall.run 在返回前
	// pop 掉它（log_service.py:248）—— 客户端看不到。
	if _, present := body["_account_email"]; present {
		t.Errorf("_account_email leaked into response: %#v", body["_account_email"])
	}

	if len(upstream.tokens) != 1 || upstream.tokens[0] != "search-token" {
		t.Errorf("factory tokens = %v", upstream.tokens)
	}
	if len(upstream.prompts) != 1 || upstream.prompts[0] != "帮我搜索 chatgpt2api" {
		t.Errorf("prompts = %v", upstream.prompts)
	}
	if len(upstream.models) != 1 || upstream.models[0] != protocol.SearchModel {
		t.Errorf("models = %v, want [%s]", upstream.models, protocol.SearchModel)
	}

	account := deps.Accts.GetAccount("search-token")
	if account == nil {
		t.Fatal("account disappeared")
	}
	if stringValueOfTest(account["last_used_at"]) == "" {
		t.Error("last_used_at not updated, MarkTextUsed missing")
	}

	// call 日志要带上账号邮箱（Python 从 _account_email 里取）。
	logs := deps.Logs.List("call", "", "", 10)
	if len(logs) != 1 {
		t.Fatalf("call logs = %d, want 1", len(logs))
	}
	detail, _ := logs[0]["detail"].(map[string]any)
	if detail["status"] != "success" {
		t.Errorf("log status = %#v", detail["status"])
	}
	if detail["account_email"] != "a@b.c" {
		t.Errorf("log account_email = %#v", detail["account_email"])
	}
	if detail["model"] != protocol.SearchModel {
		t.Errorf("log model = %#v", detail["model"])
	}
}

// 号池为空时按匿名 token 调上游（Python 的 get_text_access_token 返回空串）。
func TestSearchRouteWithoutAccountsUsesAnonymousToken(t *testing.T) {
	upstream := &fakeSearchUpstream{result: searchResultFixture()}
	server, _ := newSearchRouteServer(t, nil, upstream)

	status, payload := postJSON(t, server, "/v1/search", map[string]any{"prompt": "搜索"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	if len(upstream.tokens) != 1 || upstream.tokens[0] != "" {
		t.Errorf("factory tokens = %v, want [\"\"]", upstream.tokens)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, present := body["_account_email"]; present {
		t.Errorf("_account_email leaked into response: %#v", body["_account_email"])
	}
}

// prompt 为空返回 422，且不碰上游、不记账。
func TestSearchRouteRequiresPrompt(t *testing.T) {
	upstream := &fakeSearchUpstream{result: searchResultFixture()}
	server, deps := newSearchRouteServer(t, []map[string]any{
		{"access_token": "search-token", "email": "a@b.c", "type": "plus", "status": service.StatusNormal},
	}, upstream)

	status, payload := postJSON(t, server, "/v1/search", map[string]any{"prompt": "   "})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	if len(upstream.tokens) != 0 || len(upstream.prompts) != 0 {
		t.Errorf("upstream called: tokens=%v prompts=%v", upstream.tokens, upstream.prompts)
	}
	if account := deps.Accts.GetAccount("search-token"); stringValueOfTest(account["last_used_at"]) != "" {
		t.Error("MarkTextUsed ran on a rejected request")
	}
}

// 上游失败映射成 502，且不记账、响应不回显 _account_email。
func TestSearchRouteUpstreamErrorIs502(t *testing.T) {
	upstream := &fakeSearchUpstream{err: errors.New("upstream exploded")}
	server, deps := newSearchRouteServer(t, []map[string]any{
		{"access_token": "search-token", "email": "a@b.c", "type": "plus", "status": service.StatusNormal},
	}, upstream)

	status, payload := postJSON(t, server, "/v1/search", map[string]any{"prompt": "搜索"})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	if !strings.Contains(string(payload), "upstream exploded") {
		t.Errorf("body = %s", payload)
	}
	if account := deps.Accts.GetAccount("search-token"); stringValueOfTest(account["last_used_at"]) != "" {
		t.Error("MarkTextUsed ran on a failed search")
	}
	logs := deps.Logs.List("call", "", "", 10)
	if len(logs) != 1 {
		t.Fatalf("call logs = %d, want 1", len(logs))
	}
	detail, _ := logs[0]["detail"].(map[string]any)
	if detail["status"] != "failed" {
		t.Errorf("log status = %#v", detail["status"])
	}
}

// 鉴权失败必须 401，且不触碰上游。
func TestSearchRouteRequiresAuth(t *testing.T) {
	upstream := &fakeSearchUpstream{result: searchResultFixture()}
	deps := newTestDeps(t)
	deps.SearchUpstream = func(ctx context.Context, accessToken string) (protocol.SearchUpstream, func(), error) {
		t.Fatal("factory must not be called without auth")
		return upstream, func() {}, nil
	}
	server := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(server.Close)

	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/search",
		bytes.NewReader([]byte(`{"prompt":"搜索"}`)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", response.StatusCode, payload)
	}
}

// 敏感词在请求入口就被拦下（Python 的 filter_or_log），不消耗上游。
func TestSearchRouteBlocksSensitiveWords(t *testing.T) {
	upstream := &fakeSearchUpstream{result: searchResultFixture()}
	deps := newTestDeps(t)
	deps.ContentFilter = service.NewContentFilter(service.ContentFilterOptions{
		SensitiveWords: func() []string { return []string{"违禁内容"} },
		AIReview:       func() map[string]any { return map[string]any{"enabled": false} },
	})
	deps.SearchUpstream = func(ctx context.Context, accessToken string) (protocol.SearchUpstream, func(), error) {
		upstream.tokens = append(upstream.tokens, accessToken)
		return upstream, func() {}, nil
	}
	server := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(server.Close)

	status, payload := postJSON(t, server, "/v1/search", map[string]any{"prompt": "搜索违禁内容"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	if !strings.Contains(string(payload), "敏感词") {
		t.Errorf("body = %q", payload)
	}
	if len(upstream.tokens) != 0 {
		t.Errorf("upstream called with %v", upstream.tokens)
	}
}
