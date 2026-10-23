package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"

	"github.com/basketikun/chatgpt2api/internal/protocol"
)

// 让 *Client 满足 protocol.Upstream 接口。
var _ protocol.Upstream = (*Client)(nil)

// DoStream 发起一个 SSE 请求并返回响应体。
func (c *Client) DoStream(ctx context.Context, path string, payload []byte, requirements protocol.Requirements) (io.ReadCloser, error) {
	extra := map[string]string{
		"Accept":       "text/event-stream",
		"Content-Type": "application/json",
		"OpenAI-Sentinel-Chat-Requirements-Token": requirements.Token,
	}
	if requirements.ProofToken != "" {
		extra["OpenAI-Sentinel-Proof-Token"] = requirements.ProofToken
	}
	if requirements.TurnstileToken != "" {
		extra["OpenAI-Sentinel-Turnstile-Token"] = requirements.TurnstileToken
	}
	if requirements.SOToken != "" {
		extra["OpenAI-Sentinel-SO-Token"] = requirements.SOToken
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+path, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header = c.headers(path, extra)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("%s failed: status=%d body=%s", path, resp.StatusCode, truncate(string(body), 300))
	}
	return resp.Body, nil
}

// Authenticated 报告当前是否登录态。
func (c *Client) Authenticated() bool { return c.accessToken != "" }

// UploadImage 上传图片到上游并返回 file_id / 尺寸。
//
// 复刻 _upload_image：POST /backend-api/files 拿 upload_url，
// 再把字节 PUT 上去，最后回传 file_id 与尺寸。
func (c *Client) UploadImage(payload []byte, filename, mime string) (map[string]any, error) {
	if c.accessToken == "" {
		return nil, fmt.Errorf("authenticated upstream account required for image upload")
	}
	if mime == "" {
		mime = "image/png"
	}

	requestBody, err := json.Marshal(map[string]any{
		"file_name":           filename,
		"file_size":           len(payload),
		"use_case":            "multimodal",
		"timezone_offset_min": -480,
		"reset_rate_limits":   false,
	})
	if err != nil {
		return nil, err
	}

	path := "/backend-api/files"
	headers := c.headers(path, map[string]string{"Content-Type": "application/json"})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, BaseURL+path, strings.NewReader(string(requestBody)))
	if err != nil {
		return nil, err
	}
	req.Header = headers

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("file create failed: status=%d body=%s", resp.StatusCode, truncate(string(body), 300))
	}

	var created map[string]any
	if err := json.Unmarshal(body, &created); err != nil {
		return nil, fmt.Errorf("file create: invalid json: %w", err)
	}
	fileID := stringOfValue(created["file_id"])
	uploadURL := stringOfValue(created["upload_url"])
	if fileID == "" || uploadURL == "" {
		return nil, fmt.Errorf("file create: missing file_id or upload_url")
	}

	putReq, err := http.NewRequestWithContext(context.Background(), http.MethodPut, uploadURL, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	putReq.Header = http.Header{
		"Content-Type":      []string{mime},
		"x-ms-blob-type":    []string{"BlockBlob"},
		http.HeaderOrderKey: []string{"content-type", "x-ms-blob-type"},
	}
	putResp, err := c.http.Do(putReq)
	if err != nil {
		return nil, err
	}
	io.Copy(io.Discard, io.LimitReader(putResp.Body, 4096))
	putResp.Body.Close()
	if putResp.StatusCode >= 400 {
		return nil, fmt.Errorf("file upload failed: status=%d", putResp.StatusCode)
	}

	// 上传完成后要标记为已使用，否则上游会把它当临时文件清掉。
	finishPath := "/backend-api/files/" + fileID + "/uploaded"
	finishBody := []byte(`{}`)
	finishReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, BaseURL+finishPath, strings.NewReader(string(finishBody)))
	if err == nil {
		finishReq.Header = c.headers(finishPath, map[string]string{"Content-Type": "application/json"})
		if finishResp, finishErr := c.http.Do(finishReq); finishErr == nil {
			io.Copy(io.Discard, io.LimitReader(finishResp.Body, 4096))
			finishResp.Body.Close()
		}
	}

	width, height, _ := imageDimensions(payload)
	return map[string]any{
		"file_id":         fileID,
		"file_name":       filename,
		"file_size":       len(payload),
		"mime_type":       mime,
		"width":           width,
		"height":          height,
		"library_file_id": stringOfValue(created["library_file_id"]),
	}, nil
}

// 确保 time 包被用到（DoStream 的 ctx 超时由调用方控制，这里留作未来扩展）。
var _ = time.Second
