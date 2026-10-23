package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CtxLike 是 context.Context 的别名，避免在签名里写长类型。
type CtxLike = context.Context

// httpGetJSON 发一次带自定义头的 GET，返回 body 与状态码。
func httpGetJSON(ctx context.Context, target string, headers map[string]string, timeout time.Duration) ([]byte, int, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// httpPostJSON 发一次 JSON POST。
func httpPostJSON(ctx context.Context, target string, headers map[string]string, payload []byte, timeout time.Duration) ([]byte, int, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(payload)))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// randomHex 生成 n 字节的十六进制串。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}

func trimRightSlash(value string) string { return strings.TrimRight(strings.TrimSpace(value), "/") }

func trimSpaceString(value string) string { return strings.TrimSpace(value) }

func urlEncode(value string) string { return url.QueryEscape(value) }
