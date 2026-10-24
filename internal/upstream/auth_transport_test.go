package upstream

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

// authTransportFake 模拟 tls-client：记录请求 + 可编程响应，
// 并且真的走一遍 cookie jar（SetSessionCookie 的行为要能被断言）。
type authTransportFake struct {
	tls_client.HttpClient
	jar      tls_client.CookieJar
	requests []authTransportRequest
	response captureResponse
	finalURL string
}

type authTransportRequest struct {
	url    string
	method string
	header http.Header
	body   []byte
}

var _ tls_client.HttpClient = (*authTransportFake)(nil)

func (f *authTransportFake) SetCookies(u *url.URL, cookies []*http.Cookie) {
	f.jar.SetCookies(u, cookies)
}

func (f *authTransportFake) Cookies(u *url.URL) []*http.Cookie {
	return f.jar.Cookies(u)
}

func (f *authTransportFake) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	f.requests = append(f.requests, authTransportRequest{
		url: req.URL.String(), method: req.Method, header: req.Header, body: body,
	})
	resp := captureResponseToHTTP(f.response)
	// doFull 从 resp.Request.URL 读最终 URL，这里模拟「跟随重定向之后」。
	finalURL := f.finalURL
	if finalURL == "" {
		finalURL = req.URL.String()
	}
	parsed, _ := url.Parse(finalURL)
	resp.Request = &http.Request{URL: parsed}
	return resp, nil
}

func newAuthTransportFake() *authTransportFake {
	return &authTransportFake{jar: tls_client.NewCookieJar()}
}

// AuthGet 必须返回跟随重定向后的最终 URL（authorize 靠它分辨 /log-in/password 与 /error）。
func TestAuthGetReturnsFinalURL(t *testing.T) {
	fake := newAuthTransportFake()
	fake.response = captureResponse{body: "<html>ok</html>", contentType: "text/html"}
	fake.finalURL = "https://auth.openai.com/log-in/password"
	client := &Client{http: fake, profile: defaultProfile}

	body, status, finalURL, err := client.AuthGet(context.Background(),
		"https://auth.openai.com/api/accounts/authorize?x=1",
		map[string]string{"Accept": "text/html", "referer": "https://platform.openai.com/"},
		5*time.Second)
	if err != nil {
		t.Fatalf("AuthGet: %v", err)
	}
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if string(body) != "<html>ok</html>" {
		t.Errorf("body = %q", body)
	}
	if finalURL != "https://auth.openai.com/log-in/password" {
		t.Errorf("finalURL = %q, want the redirected URL", finalURL)
	}
	request := fake.requests[0]
	if request.method != http.MethodGet {
		t.Errorf("method = %q, want GET", request.method)
	}
	// 认证链路的头必须按调用方给的写，不能混进 chatgpt.com 的指纹头。
	if got := headerValue(request.header, "accept"); got != "text/html" {
		t.Errorf("accept = %q", got)
	}
	if got := headerValue(request.header, "referer"); got != "https://platform.openai.com/" {
		t.Errorf("referer = %q", got)
	}
	if _, ok := request.header["x-[oi]-target-path"]; ok {
		t.Error("auth request must not carry chatgpt.com fingerprint headers")
	}
	if order, ok := request.header[http.HeaderOrderKey]; !ok || len(order) != 2 {
		t.Errorf("header order = %v, want the two provided headers", order)
	}
}

// AuthPost 原样发送 body（sentinel 用的是 text/plain，不是 application/json）。
func TestAuthPostKeepsGivenContentType(t *testing.T) {
	fake := newAuthTransportFake()
	fake.response = captureResponse{body: `{"token":"t"}`}
	client := &Client{http: fake, profile: defaultProfile}

	payload := []byte(`{"p":"x","id":"d","flow":"password_verify"}`)
	body, status, err := client.AuthPost(context.Background(), sentinelReqURL,
		map[string]string{"Content-Type": "text/plain;charset=UTF-8"}, payload, 5*time.Second)
	if err != nil {
		t.Fatalf("AuthPost: %v", err)
	}
	if status != 200 || string(body) != `{"token":"t"}` {
		t.Errorf("status/body = %d/%s", status, body)
	}
	request := fake.requests[0]
	if request.method != http.MethodPost {
		t.Errorf("method = %q, want POST", request.method)
	}
	if string(request.body) != string(payload) {
		t.Errorf("body = %s, want %s", request.body, payload)
	}
	if got := headerValue(request.header, "content-type"); got != "text/plain;charset=UTF-8" {
		t.Errorf("content-type = %q", got)
	}
}

// SetSessionCookie 必须让 cookie 对同域请求可见（oai-sc 在 .openai.com 上，
// password/verify 发往 auth.openai.com，靠的就是这一层共享）。
func TestSetSessionCookieSharedAcrossSubdomains(t *testing.T) {
	fake := newAuthTransportFake()
	client := &Client{http: fake, profile: defaultProfile}

	client.SetSessionCookie(".openai.com", "oai-sc", "0challenge")
	client.SetSessionCookie("auth.openai.com", "oai-did", "device-1")

	for _, host := range []string{"auth.openai.com", "sentinel.openai.com"} {
		parsed, _ := url.Parse("https://" + host + "/x")
		cookies := fake.Cookies(parsed)
		names := map[string]string{}
		for _, cookie := range cookies {
			names[cookie.Name] = cookie.Value
		}
		if names["oai-sc"] != "0challenge" {
			t.Errorf("%s: oai-sc = %q, want 0challenge (cookies=%v)", host, names["oai-sc"], names)
		}
	}

	// 空值不进 jar（避免把 "oai-sc=" 发出去）。
	client.SetSessionCookie(".openai.com", "oai-sc", "   ")
	parsed, _ := url.Parse("https://auth.openai.com/x")
	for _, cookie := range fake.Cookies(parsed) {
		if cookie.Name == "oai-sc" && cookie.Value != "0challenge" {
			t.Errorf("blank value overwrote oai-sc: %q", cookie.Value)
		}
	}
}

// AuthPost 只剥掉 finalURL：非 2xx 是状态码透传，不是 error（调用方按状态码映射）。
func TestAuthPostPassesStatusThrough(t *testing.T) {
	fake := newAuthTransportFake()
	fake.response = captureResponse{status: 500, body: "boom"}
	client := &Client{http: fake, profile: defaultProfile}
	_, status, err := client.AuthPost(context.Background(), "https://auth.openai.com/x",
		map[string]string{"content-type": "application/json"}, []byte(`{}`), time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != 500 {
		t.Errorf("status = %d, want 500 passthrough", status)
	}
	if !strings.Contains(string(fake.requests[0].url), "auth.openai.com") {
		t.Errorf("url = %s", fake.requests[0].url)
	}
}
