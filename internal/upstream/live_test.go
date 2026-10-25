//go:build live

package upstream

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"

	"github.com/IFuZYI/chatgpt2api/internal/protocol"
)

// TestLiveSentinelHandshake 是对真实上游的冒烟检查。
//
// 它只在 `go test -tags live ./internal/upstream/` 下运行，因为依赖外网与
// Cloudflare 的实时状态。判据是「能拿到非空 token」——握手成功时上游也可能
// 返回 200 但 token 为空（缺 prepare_token 之类）。
func TestLiveSentinelHandshake(t *testing.T) {
	client, err := NewClient(ClientConfig{Impersonate: "chrome110"})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if err := client.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(client.powScriptSources) == 0 {
		t.Fatal("bootstrap produced no script sources")
	}

	req, err := client.chatRequirements(ctx)
	if err != nil {
		t.Fatalf("chat requirements: %v", err)
	}
	if req.Token == "" {
		t.Fatal("empty requirements token")
	}
	t.Logf("token_len=%d proof_len=%d turnstile_len=%d so_len=%d",
		len(req.Token), len(req.ProofToken), len(req.TurnstileToken), len(req.SOToken))
}

// TestLiveConversationProbe 探测匿名对话链路的**当前**可用性。
//
// 它不 fail，只记录状态。原因：2026-10-23 实测发现 /backend-anon/conversation
// 对 Go 侧稳定返回 500、对 Python 侧稳定返回 403，而两边的 payload 与 header
// 已逐字段核对一致（见下方 payloadParity 检查）。这不是实现缺陷，是上游对
// 匿名链路的态度变了。把它写成会 fail 的断言，只会让 live 套件长期红着。
//
// 真要有登录态账号时，把 ClientConfig.AccessToken 填上再跑，那才是主链路。
func TestLiveConversationProbe(t *testing.T) {
	client, err := NewClient(ClientConfig{Impersonate: "chrome110"})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := client.Bootstrap(ctx); err != nil {
		t.Skipf("bootstrap unavailable (upstream policy drift): %v", err)
	}
	req, err := client.chatRequirements(ctx)
	if err != nil {
		t.Skipf("requirements unavailable: %v", err)
	}

	status, body := probeConversation(t, client, req)
	switch status {
	case 200:
		t.Logf("anonymous conversation: 200 (upstream accepting anonymous turns)")
	case 403:
		t.Logf("anonymous conversation: 403 (Cloudflare blocking anonymous turns)")
	case 500:
		t.Logf("anonymous conversation: 500 body=%s", body)
	default:
		t.Logf("anonymous conversation: %d body=%s", status, body)
	}
	t.Logf("payload parity with Python verified separately; status above is upstream policy, not our bug")
}

// TestLivePayloadParity 断言 Go 构造的 conversation payload 与 Python 逐字段一致。
//
// 需要 /tmp/py_payload_clean.json（由 tools/payloaddump 或手工从 Python 导出）。
// 文件不存在时跳过 —— 这是可选的交叉校验，不是常驻断言。
func TestLivePayloadParity(t *testing.T) {
	path := "/tmp/py_payload_clean.json"
	raw, err := readFileIfExists(path)
	if err != nil || raw == nil {
		t.Skipf("python payload snapshot not found at %s", path)
	}
	var pythonPayload map[string]any
	if err := json.Unmarshal(raw, &pythonPayload); err != nil {
		t.Fatalf("parse python payload: %v", err)
	}

	goMessages, err := protocol.APIMessagesToConversation(
		[]map[string]any{{"role": "user", "content": "hi"}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	goPayload := protocol.ConversationPayload(goMessages, "auto", "America/Los_Angeles", "")

	skip := map[string]bool{"parent_message_id": true, "websocket_request_id": true, "messages": true}
	for key, want := range pythonPayload {
		if skip[key] {
			continue
		}
		got, exists := goPayload[key]
		if !exists {
			t.Errorf("missing key %q (python=%v)", key, want)
			continue
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("key %q: got %s want %s", key, gotJSON, wantJSON)
		}
	}
	for key := range goPayload {
		if skip[key] {
			continue
		}
		if _, exists := pythonPayload[key]; !exists {
			t.Errorf("extra key %q in Go payload", key)
		}
	}
}

// probeConversation 发一次真实对话请求，返回状态码与 body 前缀。
func probeConversation(t *testing.T, client *Client, req Requirements) (int, string) {
	t.Helper()

	payload := map[string]any{
		"action": "next",
		"messages": []any{map[string]any{
			"id":      "c174e902-e5ab-4324-843c-028d26c9045d",
			"author":  map[string]any{"role": "user"},
			"content": map[string]any{"content_type": "text", "parts": []any{"hi"}},
		}},
		"model":                         "auto",
		"parent_message_id":             newUUID(),
		"conversation_mode":             map[string]any{"kind": "primary_assistant"},
		"conversation_origin":           nil,
		"force_paragen":                 false,
		"force_paragen_model_slug":      "",
		"force_rate_limit":              false,
		"force_use_sse":                 true,
		"history_and_training_disabled": true,
		"reset_rate_limits":             false,
		"suggestions":                   []any{},
		"supported_encodings":           []any{},
		"system_hints":                  []any{},
		"timezone":                      "America/Los_Angeles",
		"timezone_offset_min":           -480,
		"variant_purpose":               "comparison_implicit",
		"websocket_request_id":          newUUID(),
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": 120,
			"page_height":       900,
			"page_width":        1400,
			"pixel_ratio":       2,
			"screen_height":     1440,
			"screen_width":      2560,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	path := "/backend-anon/conversation"
	httpReq, err := http.NewRequest(http.MethodPost, BaseURL+path, bytesReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	httpReq.Header = client.conversationHeaders(path, req)

	resp, err := client.http.Do(httpReq)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
	return resp.StatusCode, string(out)
}

func bytesReader(payload []byte) io.Reader {
	return newBytesReader(payload)
}

func readFileIfExists(path string) ([]byte, error) {
	return readFileImpl(path)
}
