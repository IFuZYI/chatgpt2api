package upstream

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"

	"github.com/IFuZYI/chatgpt2api/internal/protocol"
)

// captureHTTPClient 是 tls_client.HttpClient 的测试替身：
// 记录所有出站请求并按路径回放响应。
//
// 嵌入接口是为了不必实现整套方法 —— 只有 Do 会被 upstream 包调用。
type captureHTTPClient struct {
	tls_client.HttpClient

	requests []*http.Request
	bodies   [][]byte

	// responses 按 URL path 前缀匹配，返回 (status, contentType, body)。
	responses map[string]captureResponse
	// fallback 在没匹配到任何前缀时使用。
	fallback captureResponse
}

type captureResponse struct {
	status      int
	contentType string
	body        string
}

func (c *captureHTTPClient) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	c.requests = append(c.requests, req)
	c.bodies = append(c.bodies, body)

	// 最长前缀优先：/backend-api/f/conversation/prepare 必须命中 prepare 的
	// 响应，而不是被更短的 /backend-api/f/conversation 抢走。
	match := c.fallback
	bestLen := -1
	for prefix, response := range c.responses {
		if strings.HasPrefix(req.URL.Path, prefix) && len(prefix) > bestLen {
			match = response
			bestLen = len(prefix)
		}
	}
	if match.status == 0 {
		match.status = 200
	}
	header := http.Header{}
	if match.contentType != "" {
		header.Set("Content-Type", match.contentType)
	}
	return &http.Response{
		StatusCode: match.status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(match.body)),
	}, nil
}

// paths 返回所有被请求的 URL path（按顺序）。
func (c *captureHTTPClient) paths() []string {
	out := make([]string, 0, len(c.requests))
	for _, req := range c.requests {
		out = append(out, req.URL.Path)
	}
	return out
}

func newCaptureClient(accessToken string) (*Client, *captureHTTPClient) {
	fake := &captureHTTPClient{responses: map[string]captureResponse{}}
	return &Client{
		http:        fake,
		profile:     defaultProfile,
		accessToken: accessToken,
		deviceID:    "device",
		sessionID:   "session",
	}, fake
}

// 测试用的最小 PNG base64（1x1 透明像素）。
const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// codex 模型必须打到 /backend-api/codex/responses，而不是 web 的 /f/conversation。
//
// 回归点：早期实现把所有图片模型都塞进 /f/conversation，codex 模型因此
// 4xx/5xx 或扣错额度（D3-C4）。
func TestStreamImageCodexRoutesToCodexResponses(t *testing.T) {
	client, fake := newCaptureClient("codex-token")
	fake.fallback = captureResponse{
		status:      200,
		contentType: "text/event-stream",
		body:        "data: {\"type\":\"response.completed\"}\n\n",
	}

	payloads, errs, err := client.StreamImage(context.Background(), imageStreamRequestForTest("codex-gpt-image-2"))
	if err != nil {
		t.Fatalf("StreamImage: %v", err)
	}
	for range payloads {
	}
	for range errs {
	}

	paths := fake.paths()
	if len(paths) != 1 || paths[0] != "/backend-api/codex/responses" {
		t.Fatalf("paths = %v, want exactly [/backend-api/codex/responses]", paths)
	}
	// codex 端点不走 sentinel 握手，也不做 prepare。
	for _, path := range paths {
		if strings.Contains(path, "sentinel") || strings.Contains(path, "f/conversation") {
			t.Errorf("codex model must not touch web/sentinel path, saw %q", path)
		}
	}
}

// 带套餐前缀的别名同样走 codex 链路（Python 用 split_image_model 判定）。
func TestStreamImagePrefixedCodexModelRoutesToCodexResponses(t *testing.T) {
	for _, model := range []string{"plus-codex-gpt-image-2", "team-codex-gpt-image-2", "pro-codex-gpt-image-2"} {
		client, fake := newCaptureClient("codex-token")
		fake.fallback = captureResponse{status: 200, contentType: "text/event-stream", body: ""}

		payloads, errs, err := client.StreamImage(context.Background(), imageStreamRequestForTest(model))
		if err != nil {
			t.Fatalf("%s: StreamImage: %v", model, err)
		}
		for range payloads {
		}
		for range errs {
		}

		if paths := fake.paths(); len(paths) != 1 || paths[0] != "/backend-api/codex/responses" {
			t.Errorf("%s: paths = %v, want [/backend-api/codex/responses]", model, paths)
		}
	}
}

// 非 codex 模型仍走 web 链路（prepare → generate），行为不能被改动。
func TestStreamImageWebModelKeepsConversationPath(t *testing.T) {
	client, fake := newCaptureClient("")
	fake.responses["/backend-anon/sentinel/chat-requirements/prepare"] = captureResponse{
		contentType: "application/json", body: `{"prepare_token":"p"}`,
	}
	fake.responses["/backend-anon/sentinel/chat-requirements/finalize"] = captureResponse{
		contentType: "application/json", body: `{"token":"t"}`,
	}
	fake.responses["/backend-api/f/conversation/prepare"] = captureResponse{
		contentType: "application/json", body: `{"conduit_token":"c"}`,
	}
	fake.responses["/backend-api/f/conversation"] = captureResponse{
		contentType: "text/event-stream", body: "data: {\"p\":\"/message\"}\n\n",
	}
	fake.fallback = captureResponse{status: 200, contentType: "text/html", body: "<html></html>"}

	payloads, errs, err := client.StreamImage(context.Background(), imageStreamRequestForTest("gpt-image-2"))
	if err != nil {
		t.Fatalf("StreamImage: %v", err)
	}
	for range payloads {
	}
	for range errs {
	}

	paths := fake.paths()
	last := paths[len(paths)-1]
	if last != "/backend-api/f/conversation" {
		t.Fatalf("last path = %q, want /backend-api/f/conversation (paths=%v)", last, paths)
	}
	for _, path := range paths {
		if strings.Contains(path, "codex/responses") {
			t.Fatalf("web model must not hit codex endpoint, paths=%v", paths)
		}
	}
}

// payload 必须逐字段对齐 Python（openai_backend_api.py:786-800）。
func TestCodexImagePayloadMatchesPythonReference(t *testing.T) {
	payload := codexImagePayload("a cat", nil, "", "")

	if payload["model"] != CodexResponsesModel {
		t.Errorf("model = %v, want %q", payload["model"], CodexResponsesModel)
	}
	if CodexResponsesModel != "gpt-5.5" {
		t.Errorf("CodexResponsesModel = %q, want gpt-5.5", CodexResponsesModel)
	}
	if payload["instructions"] != CodexResponsesInstructions {
		t.Errorf("instructions = %v", payload["instructions"])
	}
	if payload["store"] != false {
		t.Errorf("store = %v, want false", payload["store"])
	}
	if payload["stream"] != true {
		t.Errorf("stream = %v, want true", payload["stream"])
	}
	if got, want := payload["tool_choice"], map[string]any{"type": "image_generation"}; !equalJSON(got, want) {
		t.Errorf("tool_choice = %v, want %v", got, want)
	}

	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want 1 entry", payload["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	for key, want := range map[string]any{
		"type":          "image_generation",
		"model":         "gpt-image-2",
		"action":        "generate",
		"size":          "1024x1024",
		"quality":       "auto",
		"output_format": "png",
	} {
		if tool[key] != want {
			t.Errorf("tools[0].%s = %v, want %v", key, tool[key], want)
		}
	}

	input, ok := payload["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %v, want 1 message", payload["input"])
	}
	message, _ := input[0].(map[string]any)
	if message["role"] != "user" {
		t.Errorf("input[0].role = %v", message["role"])
	}
	content, _ := message["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want only input_text when no images", content)
	}
	part, _ := content[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "a cat" {
		t.Errorf("content[0] = %v", part)
	}
}

// 有参考图时 action 必须是 edit，且图片以 data URL 内联进 input。
func TestCodexImagePayloadActionEditWithImages(t *testing.T) {
	payload := codexImagePayload("make it blue", []string{tinyPNGBase64}, "2048x2048", "high")

	tools, _ := payload["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if tool["action"] != "edit" {
		t.Errorf("action = %v, want edit", tool["action"])
	}
	if tool["size"] != "2048x2048" {
		t.Errorf("size = %v, want 2048x2048 (request value wins)", tool["size"])
	}
	if tool["quality"] != "high" {
		t.Errorf("quality = %v, want high", tool["quality"])
	}

	input, _ := payload["input"].([]any)
	message, _ := input[0].(map[string]any)
	content, _ := message["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content len = %d, want 2 (text + image)", len(content))
	}
	image, _ := content[1].(map[string]any)
	if image["type"] != "input_image" {
		t.Errorf("content[1].type = %v, want input_image", image["type"])
	}
	if image["image_url"] != "data:image/png;base64,"+tinyPNGBase64 {
		t.Errorf("image_url missing data URL prefix: %v", image["image_url"])
	}
}

// 已经是 data URL 的图片不能被二次加前缀（Python: image.startswith("data:image/")）。
func TestCodexImageInputKeepsExistingDataURL(t *testing.T) {
	dataURL := "data:image/jpeg;base64,QUJD"
	content := codexImageInput("p", []string{dataURL})
	message, _ := content[0].(map[string]any)
	parts, _ := message["content"].([]any)
	image, _ := parts[1].(map[string]any)
	if image["image_url"] != dataURL {
		t.Errorf("image_url = %v, want unchanged %q", image["image_url"], dataURL)
	}
}

// 发出去的请求体必须就是 codexImagePayload 的形状（防止 handler 层改写）。
func TestStreamCodexImageSendsExpectedBody(t *testing.T) {
	client, fake := newCaptureClient("codex-token")
	fake.fallback = captureResponse{status: 200, contentType: "text/event-stream", body: ""}

	payloads, errs, err := client.StreamImage(context.Background(), imageStreamRequestForTest("codex-gpt-image-2"))
	if err != nil {
		t.Fatalf("StreamImage: %v", err)
	}
	for range payloads {
	}
	for range errs {
	}

	if len(fake.bodies) != 1 {
		t.Fatalf("requests = %d, want 1", len(fake.bodies))
	}
	var sent map[string]any
	if err := json.Unmarshal(fake.bodies[0], &sent); err != nil {
		t.Fatalf("request body is not json: %v", err)
	}
	if sent["model"] != "gpt-5.5" {
		t.Errorf("sent model = %v, want gpt-5.5", sent["model"])
	}
	if sent["instructions"] != CodexResponsesInstructions {
		t.Errorf("sent instructions = %v", sent["instructions"])
	}

	// 认证头必须是 Bearer <token>（Python 的 _codex_responses_headers）。
	if auth := headerValue(fake.requests[0].Header, "authorization"); auth != "Bearer codex-token" {
		t.Errorf("Authorization = %q, want Bearer codex-token", auth)
	}
}

// 上游 4xx 必须带状态码冒泡（供重试/换号逻辑识别）。
func TestStreamCodexImagePropagatesHTTPError(t *testing.T) {
	client, fake := newCaptureClient("codex-token")
	fake.fallback = captureResponse{status: 429, contentType: "application/json", body: `{"error":"rate limited"}`}

	if _, _, err := client.StreamImage(context.Background(), imageStreamRequestForTest("codex-gpt-image-2")); err == nil {
		t.Fatal("expected error on 429")
	} else if !strings.Contains(err.Error(), "status=429") {
		t.Errorf("error = %v, want status=429", err)
	}
}

// 没有 access token 时不能打 codex 端点（会拿到匿名 401，错误更难排查）。
func TestStreamCodexImageRequiresAccessToken(t *testing.T) {
	client, fake := newCaptureClient("")
	if _, _, err := client.StreamImage(context.Background(), imageStreamRequestForTest("codex-gpt-image-2")); err == nil {
		t.Fatal("expected error without access token")
	}
	if len(fake.requests) != 0 {
		t.Errorf("no request should be sent, got %v", fake.paths())
	}
}

func imageStreamRequestForTest(model string) protocol.ImageStreamRequest {
	return protocol.ImageStreamRequest{Prompt: "a cat", Model: model, Size: "1024x1024", Quality: "auto"}
}

// headerValue 按小写键名取值（BuildHeaders 直接用小写键写入 map，
// fhttp.Header.Get 会规范化成首字母大写而取不到）。
func headerValue(header http.Header, name string) string {
	values := header[strings.ToLower(name)]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func equalJSON(left, right any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

// codex 流必须有超时上限：没有它，上游挂住时异步任务会永远停在 running
// （codex 没有轮询阶段可以兜底超时）。对齐 Python 的 urlopen(timeout=1200)。
func TestStreamCodexImageSetsRequestDeadline(t *testing.T) {
	client, fake := newCaptureClient("codex-token")
	fake.fallback = captureResponse{status: 200, contentType: "text/event-stream", body: ""}

	payloads, errs, err := client.StreamImage(context.Background(), imageStreamRequestForTest("codex-gpt-image-2"))
	if err != nil {
		t.Fatalf("StreamImage: %v", err)
	}
	for range payloads {
	}
	for range errs {
	}

	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fake.requests))
	}
	ctx := fake.requests[0].Context()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("codex request has no deadline — an upstream hang would pin the task in running forever")
	}
	remaining := time.Until(deadline)
	if remaining > codexResponsesTimeout || remaining < codexResponsesTimeout-30*time.Second {
		t.Errorf("deadline is %.0fs out, want ~%v", remaining.Seconds(), codexResponsesTimeout)
	}
}

// 保证 fake 满足 tls_client.HttpClient（编译期检查）。
var _ tls_client.HttpClient = (*captureHTTPClient)(nil)
