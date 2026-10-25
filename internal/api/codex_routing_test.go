package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/api"
	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/IFuZYI/chatgpt2api/internal/service"
)

// codexRouteUpstream 记录「被选中的账号 token」与送进上游的请求，并回放一个
// 带内联 base64 结果的 codex 事件（/codex/responses 的响应形态）。
type codexRouteUpstream struct {
	mu       sync.Mutex
	tokens   []string
	requests []protocol.ImageStreamRequest
}

func (u *codexRouteUpstream) ChatRequirements(ctx context.Context) (protocol.Requirements, error) {
	return protocol.Requirements{Token: "t"}, nil
}

func (u *codexRouteUpstream) DoStream(ctx context.Context, path string, payload []byte, req protocol.Requirements) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (u *codexRouteUpstream) Authenticated() bool { return true }

func (u *codexRouteUpstream) UploadImage(payload []byte, filename, mime string) (map[string]any, error) {
	return map[string]any{"file_id": "file-1"}, nil
}

func (u *codexRouteUpstream) GetConversation(ctx context.Context, conversationID string) (map[string]any, error) {
	return map[string]any{}, nil
}

func (u *codexRouteUpstream) QueryTasks(ctx context.Context, conversationID string) ([]map[string]any, error) {
	return nil, nil
}

func (u *codexRouteUpstream) ResolveImageURLs(ctx context.Context, conversationID string, fileIDs, sedimentIDs []string) ([]string, error) {
	return nil, nil
}

func (u *codexRouteUpstream) DownloadImage(ctx context.Context, url string) ([]byte, error) {
	return nil, nil
}

func (u *codexRouteUpstream) FindConversationByPrompt(ctx context.Context, prompt string, since time.Time) (string, error) {
	return "", nil
}

func (u *codexRouteUpstream) DeleteConversation(ctx context.Context, conversationID string) error {
	return nil
}

// StreamImage 回放 codex 形态的事件：结果直接内联在 image_generation_call 里。
func (u *codexRouteUpstream) StreamImage(ctx context.Context, request protocol.ImageStreamRequest) (<-chan string, <-chan error, error) {
	u.mu.Lock()
	u.requests = append(u.requests, request)
	u.mu.Unlock()

	payloads := make(chan string, 2)
	errs := make(chan error, 1)
	payloads <- `{"type":"response.created"}`
	payloads <- `{"type":"response.output_item.done","item":{"type":"image_generation_call","result":"` + tinyPNGForAPITest + `"}}`
	close(payloads)
	close(errs)
	return payloads, errs, nil
}

// tinyPNGForAPITest 是 1x1 透明 PNG 的 base64。
const tinyPNGForAPITest = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func (u *codexRouteUpstream) snapshot() ([]string, []protocol.ImageStreamRequest) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string{}, u.tokens...), append([]protocol.ImageStreamRequest{}, u.requests...)
}

// newCodexRouteServer 建一个注入了捕获式上游的测试服务器，并按给定账号初始化号池。
func newCodexRouteServer(t *testing.T, accounts []map[string]any) (*httptest.Server, *codexRouteUpstream) {
	t.Helper()
	deps := newTestDeps(t)
	if len(accounts) > 0 {
		deps.Accts.AddAccountItems(accounts, nil)
	}

	upstream := &codexRouteUpstream{}
	deps.ImageUpstream = func(ctx context.Context, accessToken string) (protocol.ImageUpstream, func(), error) {
		upstream.mu.Lock()
		upstream.tokens = append(upstream.tokens, accessToken)
		upstream.mu.Unlock()
		return upstream, func() {}, nil
	}
	deps.RegisterImageTaskRunners()

	server := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(server.Close)
	return server, upstream
}

// codex 模型只能拿 source_type=codex 的号：Codex 端点用 Codex 那份额度，
// 拿 web 号去打会 401/403 或扣错额度。
//
// 回归点：早期 acquireImageGenerator 传的是空 SlotOptions，任何号都会被选中。
func TestCodexModelAcquiresCodexSourceAccount(t *testing.T) {
	server, upstream := newCodexRouteServer(t, []map[string]any{
		{"access_token": "web-token", "type": "plus", "source_type": "web", "status": service.StatusNormal, "quota": 10},
		{"access_token": "codex-token", "type": "plus", "source_type": "codex", "status": service.StatusNormal, "quota": 10},
	})

	status, body := postJSON(t, server, "/v1/images/generations", map[string]any{
		"prompt": "a cat",
		"model":  "codex-gpt-image-2",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}

	tokens, requests := upstream.snapshot()
	if len(tokens) != 1 || tokens[0] != "codex-token" {
		t.Fatalf("selected tokens = %v, want exactly [codex-token]", tokens)
	}
	if len(requests) != 1 {
		t.Fatalf("stream requests = %d, want 1", len(requests))
	}
	if requests[0].Model != "codex-gpt-image-2" {
		t.Errorf("request model = %q", requests[0].Model)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("response not json: %v", err)
	}
	data, _ := decoded["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data = %v, want 1 image", decoded["data"])
	}
	entry, _ := data[0].(map[string]any)
	if entry["b64_json"] != tinyPNGForAPITest {
		t.Errorf("b64_json = %v, want the inline result", entry["b64_json"])
	}
}

// 带套餐前缀的别名同样只挑 codex 号（且套餐前缀不参与号池过滤时仍能取到号）。
func TestPrefixedCodexModelAcquiresCodexAccount(t *testing.T) {
	server, upstream := newCodexRouteServer(t, []map[string]any{
		{"access_token": "codex-team", "type": "team", "source_type": "codex", "status": service.StatusNormal, "quota": 10},
		{"access_token": "web-team", "type": "team", "source_type": "web", "status": service.StatusNormal, "quota": 10},
	})

	status, body := postJSON(t, server, "/v1/images/generations", map[string]any{
		"prompt": "a cat",
		"model":  "team-codex-gpt-image-2",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	tokens, _ := upstream.snapshot()
	if len(tokens) != 1 || tokens[0] != "codex-team" {
		t.Fatalf("selected tokens = %v, want [codex-team]", tokens)
	}
}

// 只有 web 号时，codex 请求必须明确报配额不足（而不是偷偷用 web 号）。
//
// 状态码是 502 而不是 429：Python 的 no-quota 文案带 source_type 前缀后是
// "no available codex image quota"，raise_image_quota_error 只匹配
// "no available image quota"，于是走 ImageGenerationError 的默认 502。
// 这里刻意保持同一行为（文案与状态码都要对齐）。
func TestCodexModelWithoutCodexAccountReportsQuota(t *testing.T) {
	server, upstream := newCodexRouteServer(t, []map[string]any{
		{"access_token": "web-token", "type": "plus", "source_type": "web", "status": service.StatusNormal, "quota": 10},
	})

	status, body := postJSON(t, server, "/v1/images/generations", map[string]any{
		"prompt": "a cat",
		"model":  "codex-gpt-image-2",
	})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", status, body)
	}
	if !strings.Contains(string(body), "no available codex image quota") {
		t.Fatalf("body = %s, want the codex quota message", body)
	}
	if tokens, _ := upstream.snapshot(); len(tokens) != 0 {
		t.Fatalf("upstream must not be called without a codex account, tokens=%v", tokens)
	}
}

// 非 codex 模型仍按原逻辑取号（不能因新增 source_type 过滤而拿不到号）。
func TestWebModelStillAcquiresWebAccount(t *testing.T) {
	server, upstream := newCodexRouteServer(t, []map[string]any{
		{"access_token": "web-token", "type": "plus", "source_type": "web", "status": service.StatusNormal, "quota": 10},
	})

	// fake 上游返回的是 codex 形态事件，web 路径解析不出 file_id，最终会以
	// message 收尾；这里只断言「取到了 web 号」。
	postJSON(t, server, "/v1/images/generations", map[string]any{
		"prompt": "a cat",
		"model":  "gpt-image-2",
	})

	tokens, requests := upstream.snapshot()
	if len(tokens) != 1 || tokens[0] != "web-token" {
		t.Fatalf("selected tokens = %v, want [web-token]", tokens)
	}
	if len(requests) != 1 {
		t.Fatalf("stream requests = %d, want 1", len(requests))
	}
	// web 链路必须把尺寸/质量拼进提示词（codex 链路则不会，它走 tools[0]）。
	// 请求里没传 size/quality 时默认是空 size + "auto"，因此只能断言质量提示。
	if !strings.Contains(requests[0].Prompt, "a cat") {
		t.Errorf("prompt = %q, want the user prompt", requests[0].Prompt)
	}
	if !strings.Contains(requests[0].Prompt, "输出图片质量为 auto。") {
		t.Errorf("prompt = %q, want the quality hint web chain appends", requests[0].Prompt)
	}
}
