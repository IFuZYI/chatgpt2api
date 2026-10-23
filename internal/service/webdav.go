package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WebDAVClient 是 WebDAV 存储的最小客户端（PUT/GET/DELETE/MKCOL/PROPFIND）。
//
// 只实现用到的五个方法：不引第三方库是因为协议面很窄，
// 而依赖会让镜像变大且引入它自己的代理/超时语义。
type WebDAVClient struct {
	BaseURL  string
	Username string
	Password string
	RootPath string
	Client   *http.Client
}

// NewWebDAVClient 从图片存储配置构造客户端。
func NewWebDAVClient(settings map[string]any) *WebDAVClient {
	return &WebDAVClient{
		BaseURL:  strings.TrimRight(strings.TrimSpace(stringValue(settings["webdav_url"])), "/"),
		Username: strings.TrimSpace(stringValue(settings["webdav_username"])),
		Password: strings.TrimSpace(stringValue(settings["webdav_password"])),
		RootPath: strings.Trim(strings.TrimSpace(stringValue(settings["webdav_root_path"])), "/"),
		Client:   &http.Client{Timeout: 60 * time.Second},
	}
}

// remoteURL 拼接远端路径（逐段转义，保留 /）。
func (c *WebDAVClient) remoteURL(rel string) string {
	parts := make([]string, 0, 4)
	if c.RootPath != "" {
		parts = append(parts, strings.Split(c.RootPath, "/")...)
	}
	if rel != "" {
		parts = append(parts, strings.Split(strings.Trim(rel, "/"), "/")...)
	}
	encoded := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			encoded = append(encoded, url.PathEscape(part))
		}
	}
	if len(encoded) == 0 {
		return c.BaseURL
	}
	return c.BaseURL + "/" + strings.Join(encoded, "/")
}

func (c *WebDAVClient) do(ctx context.Context, method, target string, body io.Reader) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	return c.Client.Do(req)
}

// newRequest 构造带 Basic Auth 的请求（需要额外改 header 的调用方用它）。
func (c *WebDAVClient) newRequest(ctx context.Context, method, target string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if c.Username != "" || c.Password != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	return req, nil
}

// ensureDirs 逐级 MKCOL（已存在会返回 405，视为成功）。
func (c *WebDAVClient) ensureDirs(ctx context.Context, rel string) error {
	segments := make([]string, 0, 4)
	if c.RootPath != "" {
		segments = append(segments, strings.Split(c.RootPath, "/")...)
	}
	if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." && dir != "" {
		segments = append(segments, strings.Split(dir, "/")...)
	}

	current := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		current = append(current, segment)
		target := c.BaseURL + "/" + strings.Join(escapeSegments(current), "/")
		resp, err := c.do(ctx, "MKCOL", target, nil)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		// 405 = 已存在；409 = 父目录不存在（理论上不会，因为我们是逐级的）。
		if resp.StatusCode >= 400 && resp.StatusCode != 405 {
			return fmt.Errorf("WebDAV MKCOL failed: HTTP %d", resp.StatusCode)
		}
	}
	return nil
}

func escapeSegments(segments []string) []string {
	out := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment != "" {
			out = append(out, url.PathEscape(segment))
		}
	}
	return out
}

// Put 上传一张图，返回公开 URL。
//
// 带 Content-Type：图片只以 .png 落盘（save 的命名规则），与 Python
// `put(..., content_type="image/png")` 一致。部分 WebDAV 服务端（Nextcloud
// 的某些版本、S3 网关）对无 Content-Type 的 PUT 会存成 application/octet-stream。
func (c *WebDAVClient) Put(ctx context.Context, rel string, payload []byte, publicBase string) (string, error) {
	if err := c.ensureDirs(ctx, rel); err != nil {
		return "", err
	}
	target := c.remoteURL(rel)
	req, err := c.newRequest(ctx, http.MethodPut, target, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "image/png")
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("WebDAV PUT failed: HTTP %d", resp.StatusCode)
	}
	if publicBase != "" {
		return strings.TrimRight(publicBase, "/") + "/" + strings.Trim(rel, "/"), nil
	}
	return target, nil
}

// Get 下载一张图。
func (c *WebDAVClient) Get(ctx context.Context, rel string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, c.remoteURL(rel), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("WebDAV GET failed: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
}

// Delete 删除一张图（404 视为成功）。
func (c *WebDAVClient) Delete(ctx context.Context, rel string) (bool, error) {
	resp, err := c.do(ctx, http.MethodDelete, c.remoteURL(rel), nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("WebDAV DELETE failed: HTTP %d", resp.StatusCode)
	}
	return true, nil
}

// TestWebDAV 复刻 test_webdav：发一次 PROPFIND 验证连通与认证。
func TestWebDAV(ctx context.Context, settings map[string]any) (map[string]any, error) {
	client := NewWebDAVClient(settings)
	if client.BaseURL == "" {
		return nil, fmt.Errorf("WebDAV URL 不能为空")
	}
	target := client.remoteURL("")
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", target, strings.NewReader(
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/></d:prop></d:propfind>`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")
	if client.Username != "" || client.Password != "" {
		req.SetBasicAuth(client.Username, client.Password)
	}

	started := time.Now()
	resp, err := client.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	latency := time.Since(started).Milliseconds()

	// 207 = Multi-Status 是标准成功响应；200 也算通。
	ok := resp.StatusCode == 207 || resp.StatusCode == 200
	result := map[string]any{
		"ok":         ok,
		"status":     resp.StatusCode,
		"latency_ms": latency,
	}
	if !ok {
		result["error"] = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return result, nil
}

// SyncToWebDAV 复刻 sync_all：把本地所有图片上传到 WebDAV。
func (s *ImageService) SyncToWebDAV(ctx context.Context, settings map[string]any) (map[string]any, error) {
	client := NewWebDAVClient(settings)
	if client.BaseURL == "" {
		return nil, fmt.Errorf("WebDAV URL 不能为空")
	}
	publicBase := strings.TrimSpace(stringValue(settings["public_base_url"]))

	uploaded, skipped, failed := 0, 0, 0
	root := s.cfg.ImagesDir()

	s.indexMu.Lock()
	indexed := s.loadIndexLocked()
	s.indexMu.Unlock()

	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() || !isImageRel(info.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)

		if item, ok := indexed[relSlash]; ok && truthy(item["webdav"]) {
			skipped++
			return nil
		}
		payload, readErr := os.ReadFile(path)
		if readErr != nil {
			failed++
			return nil
		}
		remoteURL, putErr := client.Put(ctx, relSlash, payload, publicBase)
		if putErr != nil {
			failed++
			return nil
		}

		item := indexed[relSlash]
		if item == nil {
			item = map[string]any{"rel": relSlash, "path": relSlash}
		}
		item["webdav"] = true
		item["storage"] = "both"
		item["remote_url"] = remoteURL
		indexed[relSlash] = item
		uploaded++
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.indexMu.Lock()
	s.saveIndexLocked(indexed)
	s.indexMu.Unlock()

	return map[string]any{
		"uploaded": uploaded,
		"skipped":  skipped,
		"failed":   failed,
	}, nil
}
