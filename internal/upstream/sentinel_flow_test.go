package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

// fnv1a32Golden 是 utils/sentinel.py 的 _fnv1a_32 在 Python 里的实际输出。
//
// 这四组值由仓库里的 Python 实现跑出来（python3 -c 直接调那份代码），
// 是 Go 移植是否逐字一致的唯一硬证据：四轮混合常数、按**码点**迭代、
// 32 位截断任何一处写错都会在这里露出来。
func TestFnv1a32MatchesPythonGolden(t *testing.T) {
	cases := []struct{ input, want string }{
		{"", "ab3e7c0b"},
		{"abc", "1cc93dbc"},
		{"seed-abcgAAAAABpayload", "401f05b7"},
		{"中文", "39d7c2ee"},
	}
	for _, item := range cases {
		if got := fnv1a32(item.input); got != item.want {
			t.Errorf("fnv1a32(%q) = %q, want %q", item.input, got, item.want)
		}
	}
}

// requirements token 必须是 gAAAAAC + base64(JSON)，且 JSON 能还原成 18 项数组。
func TestSentinelRequirementsTokenShape(t *testing.T) {
	generator := NewSentinelTokenGenerator("device-1", "")
	token, err := generator.GenerateRequirementsToken()
	if err != nil {
		t.Fatalf("GenerateRequirementsToken: %v", err)
	}
	if !strings.HasPrefix(token, "gAAAAAC") {
		t.Fatalf("token prefix = %q, want gAAAAAC", token[:minInt(len(token), 7)])
	}
	raw, err := base64.StdEncoding.DecodeString(token[len("gAAAAAC"):])
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	var config []any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("payload is not a JSON array: %v (%s)", err, raw)
	}
	if len(config) != 18 {
		t.Fatalf("config length = %d, want 18 (sentinel.py 的数组固定 18 项)", len(config))
	}
	// data[3] 必须是 1，data[9] 是 5..50 的整数（round(uniform(5,50))）。
	if config[3] != float64(1) {
		t.Errorf("config[3] = %v, want 1", config[3])
	}
	attempt, ok := config[9].(float64)
	if !ok || attempt < 5 || attempt > 50 {
		t.Errorf("config[9] = %v, want int in [5,50]", config[9])
	}
	if integer := int(attempt); float64(integer) != attempt {
		t.Errorf("config[9] = %v, want whole number", attempt)
	}
	// config[6] / config[7] 在 Python 里是 None → JSON null。
	if config[6] != nil || config[7] != nil {
		t.Errorf("config[6],config[7] = %v,%v want nil,nil", config[6], config[7])
	}
	if config[0] != "1920x1080" {
		t.Errorf("config[0] = %v", config[0])
	}
	if config[2] != float64(4294705152) {
		t.Errorf("config[2] = %v, want 4294705152", config[2])
	}
	if config[5] != "https://sentinel.openai.com/sentinel/20260124ceb8/sdk.js" {
		t.Errorf("config[5] = %v", config[5])
	}
	if config[14] != generator.sid {
		t.Errorf("config[14] = %v, want sid %s", config[14], generator.sid)
	}
	if config[15] != "" {
		t.Errorf("config[15] = %v, want empty string", config[15])
	}
	// config[1] 是 UTC 时间字符串，格式见 _get_config。
	when, _ := config[1].(string)
	if !strings.HasSuffix(when, "GMT+0000 (Coordinated Universal Time)") {
		t.Errorf("config[1] = %q, want UTC timestamp", when)
	}
}

// generateToken 必须真的解出满足 fnv1a32(seed+payload)[:len] <= difficulty 的 i。
func TestSentinelGenerateTokenSatisfiesDifficulty(t *testing.T) {
	generator := NewSentinelTokenGenerator("device-1", "UA/1.0")
	// difficulty="0"：任何以 "0" 开头的哈希前缀都满足（约 1/16 命中）。
	token, err := generator.GenerateToken("seed-value", "0")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !strings.HasPrefix(token, "gAAAAAB") {
		t.Fatalf("token prefix = %q, want gAAAAAB", token[:minInt(len(token), 7)])
	}
	if !strings.HasSuffix(token, "~S") {
		t.Fatalf("token suffix missing: %q", token[len(token)-4:])
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(token, "gAAAAAB"), "~S")
	hash := fnv1a32("seed-value" + payload)
	if hash[:1] > "0" {
		t.Fatalf("hash %q does not satisfy difficulty 0", hash)
	}
	// payload 必须能还原成 18 项 config，且 config[3] 是尝试次数 i。
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload not base64: %v", err)
	}
	var config []any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("payload not JSON array: %v", err)
	}
	if len(config) != 18 {
		t.Fatalf("config length = %d, want 18", len(config))
	}
	// config[3] 是尝试次数 i（从 0 起），config[9] 是已耗时毫秒。
	attempt, ok := config[3].(float64)
	if !ok || attempt < 0 {
		t.Errorf("config[3] = %v, want non-negative attempt counter", config[3])
	}
	elapsed, ok := config[9].(float64)
	if !ok || elapsed < 0 {
		t.Errorf("config[9] = %v, want elapsed milliseconds", config[9])
	}
}

// 难度串为 "ff" 时前缀必然 <= "ff"，第一轮就命中（验证比较的是字符串前缀）。
func TestSentinelGenerateTokenEasyDifficulty(t *testing.T) {
	generator := NewSentinelTokenGenerator("device-1", "UA/1.0")
	token, err := generator.GenerateToken("seed", "ff")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(token, "gAAAAAB"), "~S")
	raw, _ := base64.StdEncoding.DecodeString(payload)
	var config []any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("payload not JSON array: %v", err)
	}
	if config[3] != float64(0) {
		t.Errorf("config[3] = %v, want 0 (first attempt satisfies 'ff')", config[3])
	}
}

// 解不出来时必须返回 ERROR_PREFIX + base64("null")，而不是空串。
func TestSentinelGenerateTokenFallback(t *testing.T) {
	generator := NewSentinelTokenGenerator("device-1", "UA/1.0")
	// 8 位全零前缀只有 1/16^8 的概率命中，把尝试上限压到 1 轮必然失败。
	generator.maxAttempts = 1
	token, err := generator.GenerateToken("seed", "00000000")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	// Python 的兜底值是 _b64(str(None)) → base64(`"None"`)（带引号）。
	wantSuffix := sentinelErrorPrefix + base64.StdEncoding.EncodeToString([]byte(`"None"`))
	if token != "gAAAAAB"+wantSuffix {
		t.Errorf("fallback token = %q, want gAAAAAB%s", token, wantSuffix)
	}
	// 与 Python 逐字对齐的 golden：str(None) 编码后是 Ik5vbmUi。
	if !strings.HasSuffix(token, "Ik5vbmUi") {
		t.Errorf("fallback token = %q, want the Python str(None) payload suffix", token)
	}
}

// sentinel 请求体必须逐字段是 {"p":..,"id":..,"flow":..}，且 p 是 gAAAAAC 前缀。
func TestBuildSentinelTokenRequestBody(t *testing.T) {
	fake := &sentinelFake{
		response: captureResponse{body: `{"token":"challenge-1"}`},
	}
	client := &Client{http: fake, profile: defaultProfile}

	token, cookie, err := BuildSentinelTokenForFlow(context.Background(), client, "device-1", "password_verify")
	if err != nil {
		t.Fatalf("BuildSentinelTokenForFlow: %v", err)
	}

	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fake.requests))
	}
	request := fake.requests[0]
	if request.url != sentinelReqURL {
		t.Errorf("url = %q, want %q", request.url, sentinelReqURL)
	}
	var body map[string]any
	if err := json.Unmarshal(request.body, &body); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, request.body)
	}
	if keys := len(body); keys != 3 {
		t.Errorf("body keys = %d, want exactly p/id/flow: %s", keys, request.body)
	}
	// 键序与 Python dict 一致（p,id,flow）：结构体而非 map 的理由。
	if order := sentinelJSONKeyOrder(t, request.body); strings.Join(order, ",") != "p,id,flow" {
		t.Errorf("request key order = %v, want [p id flow]", order)
	}
	p, _ := body["p"].(string)
	if !strings.HasPrefix(p, "gAAAAAC") {
		t.Errorf("p = %q, want gAAAAAC prefix", p)
	}
	if body["id"] != "device-1" {
		t.Errorf("id = %v, want device-1", body["id"])
	}
	if body["flow"] != "password_verify" {
		t.Errorf("flow = %v, want password_verify", body["flow"])
	}

	// 请求头逐字段断言（Python 里是 data= + 这组头）。
	wantHeaders := map[string]string{
		"content-type":       "text/plain;charset=UTF-8",
		"referer":            "https://sentinel.openai.com/backend-api/sentinel/frame.html",
		"origin":             "https://sentinel.openai.com",
		"user-agent":         sentinelDefaultUserAgent,
		"sec-ch-ua":          sentinelDefaultSecChUa,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
	}
	for name, want := range wantHeaders {
		if got := headerValue(request.header, name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}

	// 返回的 header 值必须是 {"p","t","c","id","flow"}，c 是服务端 challenge。
	var sentinelValue map[string]any
	if err := json.Unmarshal([]byte(token), &sentinelValue); err != nil {
		t.Fatalf("sentinel header value not JSON: %v (%s)", err, token)
	}
	if sentinelValue["c"] != "challenge-1" {
		t.Errorf("c = %v, want challenge-1", sentinelValue["c"])
	}
	if sentinelValue["t"] != "" {
		t.Errorf("t = %v, want empty", sentinelValue["t"])
	}
	if sentinelValue["flow"] != "password_verify" || sentinelValue["id"] != "device-1" {
		t.Errorf("id/flow = %v/%v", sentinelValue["id"], sentinelValue["flow"])
	}
	if !strings.HasPrefix(stringValue(sentinelValue["p"]), "gAAAAAC") {
		t.Errorf("p without PoW should stay a requirements token: %v", sentinelValue["p"])
	}
	if cookie != "0challenge-1" {
		t.Errorf("oai-sc cookie = %q, want 0challenge-1", cookie)
	}
}

// PoW 分支：proofofwork.required=true 时 p 必须是解出来的 gAAAAAB token。
func TestBuildSentinelTokenSolvesProofOfWork(t *testing.T) {
	fake := &sentinelFake{
		response: captureResponse{body: `{"token":"challenge-2","proofofwork":{"required":true,"seed":"seed-x","difficulty":"f"}}`},
	}
	client := &Client{http: fake, profile: defaultProfile}

	token, _, err := BuildSentinelTokenForFlow(context.Background(), client, "device-1", "password_verify")
	if err != nil {
		t.Fatalf("BuildSentinelTokenForFlow: %v", err)
	}
	var sentinelValue map[string]any
	if err := json.Unmarshal([]byte(token), &sentinelValue); err != nil {
		t.Fatalf("sentinel value not JSON: %v", err)
	}
	p := stringValue(sentinelValue["p"])
	if !strings.HasPrefix(p, "gAAAAAB") {
		t.Fatalf("p = %q, want gAAAAAB prefix when PoW required", p)
	}
	if !strings.HasSuffix(p, "~S") {
		t.Errorf("p = %q, want ~S suffix", p)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(p, "gAAAAAB"), "~S")
	if hash := fnv1a32("seed-x" + payload); hash[:1] > "f" {
		t.Errorf("hash %q does not satisfy difficulty f", hash)
	}
}

// 非 200 或没有 token 时必须报错，不能返回半成品。
func TestBuildSentinelTokenRejectsBadResponse(t *testing.T) {
	cases := []struct {
		name     string
		response captureResponse
	}{
		{"http 500", captureResponse{status: 500, body: `{"token":"x"}`}},
		{"no token", captureResponse{body: `{}`}},
		// 实测踩到的形态：连发几次探测后上游回 HTML 400。
		// 早期实现只把「200 + 非 JSON」当兜底，但 400 + HTML 也会进同一条路径，
		// 于是返回 c="" 的废 token，失败被推迟成 password/verify 的 403。
		{"http 400 html", captureResponse{status: 400, body: "<html><head><title>400 Bad Request</title></head></html>"}},
		{"http 429 html", captureResponse{status: 429, body: "<html>rate limited</html>"}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			fake := &sentinelFake{response: item.response}
			client := &Client{http: fake, profile: defaultProfile}
			_, _, err := BuildSentinelTokenForFlow(context.Background(), client, "device-1", "password_verify")
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "sentinel_req_failed") {
				t.Errorf("error = %v, want sentinel_req_failed", err)
			}
		})
	}
}

// 坏 JSON 走 Python 的 except 分支：给出空 challenge 的兜底 token，且不返回 oai-sc。
func TestBuildSentinelTokenMalformedJSONFallback(t *testing.T) {
	fake := &sentinelFake{response: captureResponse{body: `not-json`}}
	client := &Client{http: fake, profile: defaultProfile}
	token, cookie, err := BuildSentinelTokenForFlow(context.Background(), client, "device-1", "password_verify")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cookie != "" {
		t.Errorf("cookie = %q, want empty on fallback", cookie)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(token), &value); err != nil {
		t.Fatalf("fallback token not JSON: %v", err)
	}
	if value["c"] != "" {
		t.Errorf("c = %v, want empty", value["c"])
	}
	if !strings.HasPrefix(stringValue(value["p"]), "gAAAAAC") {
		t.Errorf("p = %v, want requirements token", value["p"])
	}
}

// nil client 要报错而不是 panic。
func TestBuildSentinelTokenNilClient(t *testing.T) {
	if _, _, err := BuildSentinelTokenForFlow(context.Background(), nil, "d", "flow"); err == nil {
		t.Fatal("nil client should fail")
	}
}

var _ tls_client.HttpClient = (*sentinelFake)(nil)

// sentinelFake 记录发往 sentinel 端点的请求。
type sentinelFake struct {
	tls_client.HttpClient
	requests []sentinelFakeRequest
	response captureResponse
}

type sentinelFakeRequest struct {
	url    string
	header http.Header
	body   []byte
}

func (f *sentinelFake) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	f.requests = append(f.requests, sentinelFakeRequest{url: req.URL.String(), header: req.Header, body: body})
	return captureResponseToHTTP(f.response), nil
}

// sentinelJSONKeyOrder 按出现顺序返回顶层 JSON 对象的键。
func sentinelJSONKeyOrder(t *testing.T, payload []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("payload is not a JSON object: %s", payload)
	}
	order := make([]string, 0, 3)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		key, _ := token.(string)
		order = append(order, key)
		var discard any
		if err := decoder.Decode(&discard); err != nil {
			t.Fatalf("skip value: %v", err)
		}
	}
	return order
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
