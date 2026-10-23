package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// fetchFlareSolverrClearance 调 FlareSolverr 的 /v1 拿 clearance。
//
// 注意：这里刻意用标准库 http.Client 而不是带指纹的客户端——FlareSolverr 是
// 本地容器，不经过 Cloudflare。但代理由调用方传入，让 FlareSolverr 自己走代理。
func fetchFlareSolverrClearance(flaresolverrURL, targetURL, proxyURL string, timeoutSec int) *ClearanceBundle {
	flaresolverrURL = strings.TrimRight(strings.TrimSpace(flaresolverrURL), "/")
	if flaresolverrURL == "" {
		return nil
	}
	timeout := float64(maxInt(timeoutSec, 1))
	payload := map[string]any{
		"cmd":        "request.get",
		"url":        targetURL,
		"maxTimeout": int(timeout * 1000),
	}
	if normalized := normalizeProxyURL(proxyURL); normalized != "" {
		payload["proxy"] = map[string]any{"url": normalized}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}

	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	req, err := http.NewRequest(http.MethodPost, flaresolverrURL+"/v1", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return nil
	}

	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil
	}
	if strings.ToLower(stringValue(data["status"])) != "ok" {
		return nil
	}
	solution, ok := data["solution"].(map[string]any)
	if !ok {
		return nil
	}
	targetHost := hostFromURL(targetURL)
	cookies := filterFlareSolverrCookies(solution["cookies"], targetHost)
	userAgent := strings.TrimSpace(stringValue(solution["userAgent"]))
	if len(cookies) == 0 && userAgent == "" {
		return nil
	}
	return &ClearanceBundle{
		TargetHost: targetHost,
		ProxyURL:   normalizeProxyURL(proxyURL),
		Cookies:    cookies,
		UserAgent:  userAgent,
		CreatedAt:  time.Now(),
	}
}

func filterFlareSolverrCookies(raw any, targetHost string) map[string]string {
	items, ok := raw.([]any)
	if !ok {
		return map[string]string{}
	}
	out := map[string]string{}
	for _, item := range items {
		cookie, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(stringValue(cookie["name"]))
		if name == "" {
			continue
		}
		domain := strings.TrimSpace(stringValue(cookie["domain"]))
		if domain == "" || domainMatches(targetHost, domain) {
			out[name] = stringValue(cookie["value"])
		}
	}
	return out
}

// TestProxyResult 是 /api/proxy/test 的返回结构，字段名是前端契约。
type TestProxyResult struct {
	OK          bool   `json:"ok"`
	Status      int    `json:"status"`
	LatencyMS   int    `json:"latency_ms"`
	Error       any    `json:"error"`
	ProxySource string `json:"proxy_source"`
	HasProxy    bool   `json:"has_proxy"`
}

// TestProxy 复刻 Python 的 test_proxy：拿 csrf 端点验证代理连通性。
// 判定标准是 status < 500（403 也算「代理通」，因为那是 Cloudflare 的回应）。
func TestProxy(settings *ProxySettings, rawURL string, timeout time.Duration) TestProxyResult {
	candidate := normalizeProxyURL(strings.TrimSpace(rawURL))
	proxySource := "input"
	if candidate == "" {
		profile := settings.GetProfile(nil, "", false, true)
		candidate = profile.ProxyURL
		proxySource = profile.ProxySource
	}
	result := TestProxyResult{
		ProxySource: proxySource,
		HasProxy:    candidate != "",
	}
	if candidate == "" {
		result.Error = "no active proxy configured"
		return result
	}
	if !isValidProxyURL(candidate) {
		result.Error = "invalid proxy url"
		return result
	}

	transport, err := buildProxyTransport(candidate, timeout)
	if err != nil {
		result.Error = redactURLCredentials(err.Error())
		return result
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, "https://chatgpt.com/api/auth/csrf", nil)
	if err != nil {
		result.Error = redactURLCredentials(err.Error())
		return result
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (chatgpt2api proxy test)")

	started := time.Now()
	resp, err := client.Do(req)
	result.LatencyMS = int(time.Since(started).Milliseconds())
	if err != nil {
		result.Error = redactURLCredentials(err.Error())
		return result
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	result.Status = resp.StatusCode
	result.OK = resp.StatusCode < 500
	if !result.OK {
		result.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return result
}

func isValidProxyURL(raw string) bool {
	parsed, err := urlParse(normalizeProxyURL(raw))
	if err != nil {
		return false
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
		return parsed.Host != ""
	default:
		return false
	}
}

// TestClearanceResult 是 /api/proxy/clearance/test 的返回结构。
type TestClearanceResult struct {
	OK        bool   `json:"ok"`
	TargetURL string `json:"target_url"`
	LatencyMS int    `json:"latency_ms"`
	Error     any    `json:"error"`
	// Status 是语义字符串，不是 HTTP 码：
	// "ok" | "disabled" | "error" | "failed"。
	// 前端 proxy-runtime-card 在 ok=false 时直接展示 status 文案，
	// 给数字会显示成裸的 403 而不是可读原因（Python 同样用字符串）。
	Status     string         `json:"status"`
	HasCookies bool           `json:"has_cookies"`
	UserAgent  string         `json:"user_agent"`
	Runtime    map[string]any `json:"runtime"`
}

// TestClearance 复刻 Python 的 test_clearance：刷新一次 clearance 再用它请求目标。
func TestClearance(settings *ProxySettings, targetURL string) TestClearanceResult {
	target := strings.TrimSpace(targetURL)
	if target == "" {
		target = "https://chatgpt.com"
	}
	started := time.Now()
	result := TestClearanceResult{
		TargetURL: target,
		Runtime:   settings.RuntimeStatus(),
	}

	// 未启用清障时直接返回 disabled，不发网络请求。
	//
	// Python test_clearance 第一件事就是查 clearance_enabled 并早退。
	// 缺这个分支的话，未启用清障的用户点「测试」会看到
	// 「Clearance 获取失败：clearance unavailable」—— 误以为是配置错了，
	// 实际上只是功能没开。
	profile := settings.GetProfile(nil, "", false, true)
	if !profile.ClearanceEnabled() {
		result.Status = "disabled"
		result.Error = "clearance is disabled"
		return result
	}
	bundle := settings.RefreshClearance(target, profile, true)
	if bundle == nil {
		result.LatencyMS = int(time.Since(started).Milliseconds())
		result.Error = "clearance unavailable"
		result.Status = "error"
		return result
	}
	result.HasCookies = len(bundle.Cookies) > 0
	result.UserAgent = bundle.UserAgent

	transport, err := buildProxyTransport(profile.ProxyURL, 20*time.Second)
	if err != nil {
		result.LatencyMS = int(time.Since(started).Milliseconds())
		result.Error = redactURLCredentials(err.Error())
		result.Status = "error"
		return result
	}
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		result.Error = err.Error()
		result.Status = "error"
		return result
	}
	userAgent := bundle.UserAgent
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (chatgpt2api clearance test)"
	}
	req.Header.Set("User-Agent", userAgent)
	if header := bundle.CookieHeader(); header != "" {
		req.Header.Set("Cookie", header)
	}
	resp, err := client.Do(req)
	result.LatencyMS = int(time.Since(started).Milliseconds())
	if err != nil {
		result.Error = redactURLCredentials(err.Error())
		result.Status = "error"
		return result
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	result.OK = resp.StatusCode < 400
	if result.OK {
		result.Status = "ok"
	} else {
		result.Status = "failed"
		result.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return result
}
