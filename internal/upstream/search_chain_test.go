package upstream

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"

	"github.com/IFuZYI/chatgpt2api/internal/protocol"
)

// searchFake 是搜索链路的可编程上游：按路径回放响应并记录请求。
// 轮询用的 GET /backend-api/conversation/{id} 按调用次数依次取脚本，
// 最后一条会一直重复（模拟「结果不变」）。
type searchFake struct {
	tls_client.HttpClient

	requests []*searchFakeRequest

	bootstrap             captureResponse
	requirementsPrepare   captureResponse
	requirementsFinalize  captureResponse
	prepare               captureResponse
	stream                captureResponse
	conversationResponses []captureResponse
	conversationCalls     int
}

// searchFakeRequest 是一次被记录的出站请求。
type searchFakeRequest struct {
	path   string
	header http.Header
	body   []byte
}

func (f *searchFake) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	f.requests = append(f.requests, &searchFakeRequest{path: req.URL.Path, header: req.Header, body: body})

	var response captureResponse
	switch {
	case req.URL.Path == "/":
		response = f.bootstrap
	case req.URL.Path == "/backend-api/sentinel/chat-requirements/prepare":
		response = f.requirementsPrepare
	case req.URL.Path == "/backend-api/sentinel/chat-requirements/finalize":
		response = f.requirementsFinalize
	case req.URL.Path == "/backend-api/f/conversation/prepare":
		response = f.prepare
	case req.URL.Path == "/backend-api/f/conversation":
		response = f.stream
	case strings.HasPrefix(req.URL.Path, "/backend-api/conversation/"):
		response = f.nextConversation()
	default:
		response = captureResponse{status: 404, body: `{"detail":"not found"}`}
	}
	return captureResponseToHTTP(response), nil
}

// nextConversation 取下一条会话文档脚本。
func (f *searchFake) nextConversation() captureResponse {
	if len(f.conversationResponses) == 0 {
		return captureResponse{status: 500, body: `{"detail":"no conversation scripted"}`}
	}
	index := f.conversationCalls
	if index >= len(f.conversationResponses) {
		index = len(f.conversationResponses) - 1
	}
	f.conversationCalls++
	return f.conversationResponses[index]
}

func (f *searchFake) requestsFor(path string) []*searchFakeRequest {
	out := make([]*searchFakeRequest, 0, 1)
	for _, request := range f.requests {
		if request.path == path {
			out = append(out, request)
		}
	}
	return out
}

// requireSingleRequest 断言某路径只被请求一次并返回它。
func (f *searchFake) requireSingleRequest(t *testing.T, path string) *searchFakeRequest {
	t.Helper()
	requests := f.requestsFor(path)
	if len(requests) != 1 {
		t.Fatalf("%s request count = %d, want 1", path, len(requests))
	}
	return requests[0]
}

// captureResponseToHTTP 把脚本响应转成 fhttp.Response。
func captureResponseToHTTP(response captureResponse) *http.Response {
	if response.status == 0 {
		response.status = 200
	}
	header := http.Header{}
	if response.contentType != "" {
		header["content-type"] = []string{response.contentType}
	}
	return &http.Response{
		StatusCode: response.status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(response.body)),
	}
}

// newSearchFake 构造一个绑定假上游的 Client。
func newSearchFake(accessToken string) (*Client, *searchFake) {
	fake := &searchFake{
		bootstrap: captureResponse{
			contentType: "text/html",
			body:        `<html data-build="build-1"><script src="/backend-api/sentinel/sdk.js"></script></html>`,
		},
		requirementsPrepare:  captureResponse{body: `{"prepare_token":"pt-1"}`},
		requirementsFinalize: captureResponse{body: `{"token":"sentinel-token"}`},
		prepare:              captureResponse{body: `{"conduit_token":"conduit-1"}`},
		stream: captureResponse{
			contentType: "text/event-stream",
			body:        "data: {\"conversation_id\":\"cid-1\"}\n\ndata: [DONE]\n\n",
		},
	}
	client := &Client{
		http:        fake,
		profile:     defaultProfile,
		accessToken: accessToken,
		userAgent:   "test-agent",
		deviceID:    "device",
		sessionID:   "session",
	}
	return client, fake
}

// fastSearchOptions 压短超时与轮询间隔，避免测试真的等 3 秒。
func fastSearchOptions() searchOptions {
	return searchOptions{timeout: 2 * time.Second, pollInterval: time.Millisecond}
}

// searchConversationBody 造一份会话文档（assistant 消息 + 完成状态）。
func searchConversationBody(answer, status string) string {
	message := map[string]any{
		"id":          "m-1",
		"author":      map[string]any{"role": "assistant"},
		"create_time": 100.0,
		"content":     map[string]any{"content_type": "text", "parts": []any{answer}},
		"metadata":    map[string]any{"finish_details": map[string]any{"type": status}},
	}
	payload, _ := json.Marshal(map[string]any{
		"mapping": map[string]any{"node": map[string]any{"message": message}},
	})
	return string(payload)
}

// conversationResponse 包一层 JSON content-type。
func conversationResponse(body string) captureResponse {
	return captureResponse{contentType: "application/json", body: body}
}

// decodeJSONMap 解析请求体，失败直接 Fatal。
func decodeJSONMap(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, payload)
	}
	return decoded
}

var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// prepare 请求体必须与 Python json={...} 逐字段一致。
func TestSearchPrepareBodyMatchesPython(t *testing.T) {
	client, fake := newSearchFake("token-1")
	fake.conversationResponses = []captureResponse{
		conversationResponse(searchConversationBody("答案", "finished_successfully")),
	}

	if _, err := client.search(context.Background(), "帮我搜索 chatgpt2api", "", fastSearchOptions()); err != nil {
		t.Fatalf("search: %v", err)
	}

	request := fake.requireSingleRequest(t, "/backend-api/f/conversation/prepare")
	body := decodeJSONMap(t, request.body)

	partialQuery, ok := body["partial_query"].(map[string]any)
	if !ok {
		t.Fatalf("partial_query = %#v", body["partial_query"])
	}
	if id := stringValue(partialQuery["id"]); !uuidV4Re.MatchString(id) {
		t.Errorf("partial_query.id = %q, want uuid4", id)
	}
	partialQuery["id"] = "<uuid>"

	want := map[string]any{
		"action":                "next",
		"fork_from_shared_post": false,
		"parent_message_id":     "client-created-root",
		"model":                 "gpt-5-5",
		"client_prepare_state":  "success",
		"timezone_offset_min":   float64(-480),
		"timezone":              "Asia/Shanghai",
		"conversation_mode":     map[string]any{"kind": "primary_assistant"},
		"system_hints":          []any{"search"},
		"partial_query": map[string]any{
			"id":      "<uuid>",
			"author":  map[string]any{"role": "user"},
			"content": map[string]any{"content_type": "text", "parts": []any{"帮我搜索 chatgpt2api"}},
		},
		"supports_buffering":     true,
		"supported_encodings":    []any{"v1"},
		"client_contextual_info": map[string]any{"app_name": "chatgpt.com"},
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("prepare body mismatch\n got: %#v\nwant: %#v", body, want)
	}

	if got := request.header["x-conduit-token"]; len(got) != 1 || got[0] != "no-token" {
		t.Errorf("X-Conduit-Token = %v, want [no-token]", got)
	}
	if got := request.header["accept"]; len(got) != 1 || got[0] != "*/*" {
		t.Errorf("Accept = %v, want [*/*]", got)
	}
	if got := request.header["content-type"]; len(got) != 1 || got[0] != "application/json" {
		t.Errorf("Content-Type = %v", got)
	}
	// Python 的 prepare 不带 sentinel token（只有 X-Conduit-Token 占位）。
	if got, ok := request.header["openai-sentinel-chat-requirements-token"]; ok {
		t.Errorf("prepare must not send sentinel header, got %v", got)
	}
}

// conversation 请求体必须与 Python 逐字段一致（含 force_use_search）。
func TestSearchConversationBodyMatchesPython(t *testing.T) {
	client, fake := newSearchFake("token-1")
	fake.conversationResponses = []captureResponse{
		conversationResponse(searchConversationBody("答案", "finished_successfully")),
	}

	if _, err := client.search(context.Background(), "帮我搜索 chatgpt2api", "", fastSearchOptions()); err != nil {
		t.Fatalf("search: %v", err)
	}

	request := fake.requireSingleRequest(t, "/backend-api/f/conversation")
	body := decodeJSONMap(t, request.body)

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v", body["messages"])
	}
	message, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("messages[0] = %#v", messages[0])
	}
	if id := stringValue(message["id"]); !uuidV4Re.MatchString(id) {
		t.Errorf("messages[0].id = %q, want uuid4", id)
	}
	message["id"] = "<uuid>"
	createTime, ok := message["create_time"].(float64)
	if !ok || createTime <= 0 {
		t.Errorf("messages[0].create_time = %#v, want positive float", message["create_time"])
	}
	message["create_time"] = float64(0)

	want := map[string]any{
		"action": "next",
		"messages": []any{map[string]any{
			"id":          "<uuid>",
			"author":      map[string]any{"role": "user"},
			"create_time": float64(0),
			"content":     map[string]any{"content_type": "text", "parts": []any{"帮我搜索 chatgpt2api"}},
			"metadata": map[string]any{
				"developer_mode_connector_ids": []any{},
				"selected_github_repos":        []any{},
				"selected_all_github_repos":    false,
				"system_hints":                 []any{"search"},
				"serialization_metadata":       map[string]any{"custom_symbol_offsets": []any{}},
			},
		}},
		"parent_message_id":                    "client-created-root",
		"model":                                "gpt-5-5",
		"client_prepare_state":                 "success",
		"timezone_offset_min":                  float64(-480),
		"timezone":                             "Asia/Shanghai",
		"conversation_mode":                    map[string]any{"kind": "primary_assistant"},
		"enable_message_followups":             true,
		"system_hints":                         []any{},
		"supports_buffering":                   true,
		"supported_encodings":                  []any{"v1"},
		"force_use_search":                     true,
		"client_reported_search_source":        "conversation_composer_web_icon",
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": float64(36),
			"page_height":       float64(925),
			"page_width":        float64(886),
			"pixel_ratio":       float64(2),
			"screen_height":     float64(1440),
			"screen_width":      float64(2560),
			"app_name":          "chatgpt.com",
		},
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("conversation body mismatch\n got: %#v\nwant: %#v", body, want)
	}

	if got := request.header["x-conduit-token"]; len(got) != 1 || got[0] != "conduit-1" {
		t.Errorf("X-Conduit-Token = %v, want [conduit-1]", got)
	}
	if got := request.header["accept"]; len(got) != 1 || got[0] != "text/event-stream" {
		t.Errorf("Accept = %v", got)
	}
	if got := request.header["openai-sentinel-chat-requirements-token"]; len(got) != 1 || got[0] != "sentinel-token" {
		t.Errorf("sentinel token = %v", got)
	}
	if got := request.header["x-oai-turn-trace-id"]; len(got) != 1 || got[0] == "" {
		t.Errorf("X-Oai-Turn-Trace-Id = %v, want non-empty", got)
	}
}

// answer 连续两次与上次相同（共 3 次一致）就返回，不等 status 完成。
func TestSearchPollsUntilAnswerStable(t *testing.T) {
	client, fake := newSearchFake("token-1")
	stable := conversationResponse(searchConversationBody("稳定答案", ""))
	fake.conversationResponses = []captureResponse{stable, stable, stable, stable}

	result, err := client.search(context.Background(), "问题", "", fastSearchOptions())
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if fake.conversationCalls != 3 {
		t.Errorf("conversation polls = %d, want 3 (stable_hits >= 2)", fake.conversationCalls)
	}
	if answer, _ := result["answer"].(string); answer != "稳定答案" {
		t.Errorf("answer = %q", result["answer"])
	}
}

// status 命中 SEARCH_DONE_STATUS 时第一次轮询就返回。
func TestSearchReturnsOnDoneStatus(t *testing.T) {
	client, fake := newSearchFake("token-1")
	fake.conversationResponses = []captureResponse{
		conversationResponse(searchConversationBody("立即完成", "finished_successfully")),
	}

	result, err := client.search(context.Background(), "问题", "", fastSearchOptions())
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if fake.conversationCalls != 1 {
		t.Errorf("conversation polls = %d, want 1", fake.conversationCalls)
	}
	if status, _ := result["status"].(string); status != "finished_successfully" {
		t.Errorf("status = %q", result["status"])
	}
}

// 404/409/423/429/500/502/503/504 要容忍（会话刚建好时上游会短暂这样）。
func TestSearchToleratesRetryableStatus(t *testing.T) {
	client, fake := newSearchFake("token-1")
	fake.conversationResponses = []captureResponse{
		{status: 429, body: `{"detail":"rate limited"}`},
		{status: 503, body: `{"detail":"unavailable"}`},
		conversationResponse(searchConversationBody("最终答案", "finished_successfully")),
	}

	result, err := client.search(context.Background(), "问题", "", fastSearchOptions())
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if fake.conversationCalls != 3 {
		t.Errorf("conversation polls = %d, want 3", fake.conversationCalls)
	}
	if answer, _ := result["answer"].(string); answer != "最终答案" {
		t.Errorf("answer = %q", answer)
	}
}

// 401 不在容忍集合里，必须立刻冒泡（不能一直轮询到超时）。
func TestSearchPropagatesUnauthorized(t *testing.T) {
	client, fake := newSearchFake("token-1")
	fake.conversationResponses = []captureResponse{
		{status: 401, body: `{"detail":"unauthorized"}`},
	}

	_, err := client.search(context.Background(), "问题", "", fastSearchOptions())
	if err == nil {
		t.Fatal("search error = nil, want 401")
	}
	if status := protocol.StatusCodeOf(err); status != 401 {
		t.Errorf("status = %d, want 401 (err=%v)", status, err)
	}
	if fake.conversationCalls != 1 {
		t.Errorf("conversation polls = %d, want 1", fake.conversationCalls)
	}
}

// 一次成功轮询都没有（全是容忍状态码）时按 timeout 报错。
//
// 注意：只要成功拉到过会话文档（哪怕 answer 为空），Python 在超时后也返回
// 那份结果而不是报错 —— 见 TestSearchTimeoutReturnsLastResult。
func TestSearchTimesOutWithoutResult(t *testing.T) {
	client, fake := newSearchFake("token-1")
	fake.conversationResponses = []captureResponse{
		{status: 500, body: `{"detail":"boom"}`},
	}

	_, err := client.search(context.Background(), "问题", "", searchOptions{
		timeout:      30 * time.Millisecond,
		pollInterval: 5 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for search result: cid-1") {
		t.Fatalf("err = %v", err)
	}
	if fake.conversationCalls == 0 {
		t.Error("conversation was never polled")
	}
}

// 超时但已经拿到过（answer 为空的）文档时返回上一份结果，与 Python 一致。
func TestSearchTimeoutReturnsLastResult(t *testing.T) {
	client, fake := newSearchFake("token-1")
	fake.conversationResponses = []captureResponse{
		conversationResponse(searchConversationBody("", "")),
		{status: 500, body: `{"detail":"boom"}`},
	}

	result, err := client.search(context.Background(), "问题", "", searchOptions{
		timeout:      30 * time.Millisecond,
		pollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if result == nil {
		t.Fatal("result = nil")
	}
	if answer, _ := result["answer"].(string); answer != "" {
		t.Errorf("answer = %q, want empty", answer)
	}
}

// 轮询 GET 的 Referer 与 target-route 必须是 Python 的字面量约定。
func TestSearchPollRequestHeaders(t *testing.T) {
	client, fake := newSearchFake("token-1")
	fake.conversationResponses = []captureResponse{
		conversationResponse(searchConversationBody("答案", "finished_successfully")),
	}

	if _, err := client.search(context.Background(), "问题", "", fastSearchOptions()); err != nil {
		t.Fatalf("search: %v", err)
	}

	request := fake.requireSingleRequest(t, "/backend-api/conversation/cid-1")
	if got := request.header["referer"]; len(got) != 1 || got[0] != BaseURL+"/c/cid-1" {
		t.Errorf("Referer = %v, want [%s/c/cid-1]", got, BaseURL)
	}
	// 注意是字面量 {conversation_id}，不是真实 id（Python 同样写法）。
	if got := request.header["x-openai-target-route"]; len(got) != 1 ||
		got[0] != "/backend-api/conversation/{conversation_id}" {
		t.Errorf("X-OpenAI-Target-Route = %v", got)
	}
	if got := request.header["accept"]; len(got) != 1 || got[0] != "*/*" {
		t.Errorf("Accept = %v", got)
	}
	if got := request.header["authorization"]; len(got) != 1 || got[0] != "Bearer token-1" {
		t.Errorf("Authorization = %v", got)
	}
}

func TestSearchRequiresAccessToken(t *testing.T) {
	client, fake := newSearchFake("")

	_, err := client.search(context.Background(), "问题", "", fastSearchOptions())
	if err == nil || !strings.Contains(err.Error(), "access_token is required for search") {
		t.Fatalf("err = %v", err)
	}
	if len(fake.requests) != 0 {
		t.Errorf("requests = %d, want 0", len(fake.requests))
	}
}

func TestSearchMissingConduitToken(t *testing.T) {
	client, fake := newSearchFake("token-1")
	fake.prepare = captureResponse{body: `{}`}

	_, err := client.search(context.Background(), "问题", "", fastSearchOptions())
	if err == nil || !strings.Contains(err.Error(), "missing conduit_token") {
		t.Fatalf("err = %v", err)
	}
	if requests := fake.requestsFor("/backend-api/f/conversation"); len(requests) != 0 {
		t.Errorf("conversation request count = %d, want 0", len(requests))
	}
}

func TestSearchMissingConversationIDInStream(t *testing.T) {
	client, fake := newSearchFake("token-1")
	fake.stream = captureResponse{
		contentType: "text/event-stream",
		body:        "data: {\"type\":\"delta\"}\n\ndata: [DONE]\n\n",
	}

	_, err := client.search(context.Background(), "问题", "", fastSearchOptions())
	if err == nil || !strings.Contains(err.Error(), "conversation_id not found in stream") {
		t.Fatalf("err = %v", err)
	}
	if fake.conversationCalls != 0 {
		t.Errorf("conversation polls = %d, want 0", fake.conversationCalls)
	}
}

func TestSearchPrepareHTTPErrorBubbles(t *testing.T) {
	client, fake := newSearchFake("token-1")
	fake.prepare = captureResponse{status: 500, body: `{"detail":"boom"}`}

	_, err := client.search(context.Background(), "问题", "", fastSearchOptions())
	if err == nil {
		t.Fatal("err = nil, want 500")
	}
	if status := protocol.StatusCodeOf(err); status != 500 {
		t.Errorf("status = %d, want 500", status)
	}
}

// 空的 model 必须回落到 protocol.SearchModel（Python 的默认参数）。
func TestSearchDefaultsModelToSearchModel(t *testing.T) {
	client, fake := newSearchFake("token-1")
	fake.conversationResponses = []captureResponse{
		conversationResponse(searchConversationBody("答案", "finished_successfully")),
	}

	if _, err := client.search(context.Background(), "问题", "", fastSearchOptions()); err != nil {
		t.Fatalf("search: %v", err)
	}
	prepare := decodeJSONMap(t, fake.requireSingleRequest(t, "/backend-api/f/conversation/prepare").body)
	if model, _ := prepare["model"].(string); model != protocol.SearchModel {
		t.Errorf("model = %q, want %q", model, protocol.SearchModel)
	}
}
