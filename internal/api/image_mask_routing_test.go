package api_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/api"
	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/basketikun/chatgpt2api/internal/service"
)

// maskCaptureUpstream 记录送进上游的参考图，用于断言 mask 真的被合成了。
type maskCaptureUpstream struct {
	mu     sync.Mutex
	images [][3]string
	prompt string
	calls  int
}

func (u *maskCaptureUpstream) ChatRequirements(ctx context.Context) (protocol.Requirements, error) {
	return protocol.Requirements{Token: "t"}, nil
}

func (u *maskCaptureUpstream) DoStream(ctx context.Context, path string, payload []byte, req protocol.Requirements) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (u *maskCaptureUpstream) Authenticated() bool { return true }

func (u *maskCaptureUpstream) UploadImage(payload []byte, filename, mime string) (map[string]any, error) {
	return map[string]any{"file_id": "file-1"}, nil
}

func (u *maskCaptureUpstream) GetConversation(ctx context.Context, conversationID string) (map[string]any, error) {
	return map[string]any{}, nil
}

func (u *maskCaptureUpstream) QueryTasks(ctx context.Context, conversationID string) ([]map[string]any, error) {
	return nil, nil
}

func (u *maskCaptureUpstream) ResolveImageURLs(ctx context.Context, conversationID string, fileIDs, sedimentIDs []string) ([]string, error) {
	return nil, nil
}

func (u *maskCaptureUpstream) DownloadImage(ctx context.Context, url string) ([]byte, error) {
	return nil, nil
}

func (u *maskCaptureUpstream) FindConversationByPrompt(ctx context.Context, prompt string, since time.Time) (string, error) {
	return "", nil
}

func (u *maskCaptureUpstream) DeleteConversation(ctx context.Context, conversationID string) error {
	return nil
}

// StreamImage 只记录入参就结束流：断言点在「送上游的图」而不是生成结果。
func (u *maskCaptureUpstream) StreamImage(ctx context.Context, request protocol.ImageStreamRequest) (<-chan string, <-chan error, error) {
	u.mu.Lock()
	u.calls++
	u.images = append([][3]string{}, request.Images...)
	u.prompt = request.Prompt
	u.mu.Unlock()

	payloads := make(chan string, 1)
	errs := make(chan error, 1)
	payloads <- `{"p":"/message/content/parts/0","o":"append","v":"ok"}`
	close(payloads)
	close(errs)
	return payloads, errs, nil
}

func (u *maskCaptureUpstream) snapshot() ([][3]string, int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][3]string{}, u.images...), u.calls
}

// seedImageAccount 往号池放一个可用账号，让图片链路能拿到槽位。
func seedImageAccount(t *testing.T, deps *api.Deps) {
	t.Helper()
	deps.Accts.AddAccountItems([]map[string]any{
		{"access_token": "token-1", "type": "free", "status": service.StatusNormal, "quota": 10},
	}, nil)
}

// newMaskTestServer 建一个注入了捕获式上游的测试服务器。
func newMaskTestServer(t *testing.T) (*httptest.Server, *maskCaptureUpstream, *api.Deps) {
	t.Helper()
	deps := newTestDeps(t)
	seedImageAccount(t, deps)

	upstream := &maskCaptureUpstream{}
	deps.ImageUpstream = func(ctx context.Context, accessToken string) (protocol.ImageUpstream, func(), error) {
		return upstream, func() {}, nil
	}
	deps.RegisterImageTaskRunners()

	server := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(server.Close)
	return server, upstream, deps
}

// callLogs 取全部 call 类型日志（最新在前）。
func callLogs(deps *api.Deps) []map[string]any {
	return deps.Logs.List("call", "", "", 50)
}

// solidPNGDataURL 造一张纯色 PNG 的 data URL。
func solidPNGDataURL(t *testing.T, width, height int, c color.NRGBA) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// halfMaskDataURL 造一张左半透明（要编辑）、右半不透明的 RGBA mask。
func halfMaskDataURL(t *testing.T, width, height int) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			alpha := uint8(0)
			if x >= width/2 {
				alpha = 255
			}
			img.SetNRGBA(x, y, color.NRGBA{R: 0, G: 0, B: 0, A: alpha})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// alphaOf 解出三元组里的图片并取某像素 alpha。
func alphaOf(t *testing.T, tuple [3]string, x, y int) uint8 {
	t.Helper()
	payload, err := base64.StdEncoding.DecodeString(tuple[0])
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode image: %v", err)
	}
	if format != "png" {
		t.Fatalf("uploaded image format = %q, want png (mask compositing must re-encode as PNG)", format)
	}
	_, _, _, alpha := decoded.At(x, y).RGBA()
	return uint8(alpha >> 8)
}

// postJSON 发一个带鉴权的 JSON POST。
func postJSON(t *testing.T, server *httptest.Server, path string, body map[string]any) (int, []byte) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, payload
}

// 回归点：同步编辑接口曾把 mask 直接丢弃（`_ = mask`），导致局部重绘
// 静默退化成整图重画 —— 结果「看起来对但语义错」。
//
// 断言：送上游的图必须带 mask 的 alpha 通道，且被重新编码成 PNG。
func TestImageEditSyncCompositesMaskBeforeUpstream(t *testing.T) {
	server, upstream, _ := newMaskTestServer(t)

	status, body := postJSON(t, server, "/v1/images/edits", map[string]any{
		"prompt": "把左边换成一只猫",
		"image":  solidPNGDataURL(t, 4, 2, color.NRGBA{R: 10, G: 20, B: 30, A: 255}),
		"mask":   halfMaskDataURL(t, 4, 2),
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}

	images, calls := upstream.snapshot()
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if len(images) != 1 {
		t.Fatalf("images forwarded = %d, want 1", len(images))
	}
	if images[0][2] != "image/png" {
		t.Errorf("mime = %q, want image/png", images[0][2])
	}
	// 左半透明（要编辑）→ 0；右半不透明（保留）→ 255。
	if alpha := alphaOf(t, images[0], 0, 0); alpha != 0 {
		t.Errorf("alpha at (0,0) = %d, want 0 — mask was not composited into the image", alpha)
	}
	if alpha := alphaOf(t, images[0], 3, 0); alpha != 255 {
		t.Errorf("alpha at (3,0) = %d, want 255", alpha)
	}
}

// 不带 mask 时必须原样透传（Python 的 `if not masks: return images`）。
func TestImageEditSyncWithoutMaskPassesImageThrough(t *testing.T) {
	server, upstream, _ := newMaskTestServer(t)

	source := solidPNGDataURL(t, 3, 3, color.NRGBA{R: 1, G: 2, B: 3, A: 255})
	status, body := postJSON(t, server, "/v1/images/edits", map[string]any{
		"prompt": "重新画一下",
		"image":  source,
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}

	images, _ := upstream.snapshot()
	if len(images) != 1 {
		t.Fatalf("images forwarded = %d, want 1", len(images))
	}
	want := strings.TrimPrefix(source, "data:image/png;base64,")
	if images[0][0] != want {
		t.Error("image payload should be forwarded unchanged when no mask is supplied")
	}
}

// 回归点：异步编辑链路里 taskCtx.Masks 曾经从未被读取，mask 同样被静默丢弃。
func TestImageTaskEditCompositesMask(t *testing.T) {
	server, upstream, _ := newMaskTestServer(t)

	status, body := postJSON(t, server, "/api/image-tasks/edits", map[string]any{
		"client_task_id": "task-mask-1",
		"prompt":         "把左边换成一只猫",
		"image":          solidPNGDataURL(t, 4, 2, color.NRGBA{R: 10, G: 20, B: 30, A: 255}),
		"mask":           halfMaskDataURL(t, 4, 2),
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}

	// 等任务进入终态。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		images, calls := upstream.snapshot()
		if calls > 0 {
			if len(images) != 1 {
				t.Fatalf("images forwarded = %d, want 1", len(images))
			}
			if images[0][2] != "image/png" {
				t.Errorf("mime = %q, want image/png", images[0][2])
			}
			if alpha := alphaOf(t, images[0], 0, 0); alpha != 0 {
				t.Errorf("alpha at (0,0) = %d, want 0 — taskCtx.Masks was not composited", alpha)
			}
			if alpha := alphaOf(t, images[0], 3, 0); alpha != 255 {
				t.Errorf("alpha at (3,0) = %d, want 255", alpha)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("upstream was never called by the async edit task")
}

// mask 打不开时必须报错（Python 里 PIL 抛异常 → 502），不能静默退回整图重画。
func TestImageEditSyncReportsMaskDecodeError(t *testing.T) {
	server, upstream, _ := newMaskTestServer(t)

	status, body := postJSON(t, server, "/v1/images/edits", map[string]any{
		"prompt": "把左边换成一只猫",
		"image":  solidPNGDataURL(t, 4, 2, color.NRGBA{A: 255}),
		"mask":   "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not a png")),
	})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body = %s)", status, body)
	}
	if _, calls := upstream.snapshot(); calls != 0 {
		t.Errorf("upstream calls = %d, want 0 — a broken mask must not reach upstream", calls)
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("parse error body: %v", err)
	}
	errorPayload, ok := envelope["error"].(map[string]any)
	if !ok {
		t.Fatalf("error envelope missing: %s", body)
	}
	message := fmt.Sprint(errorPayload["message"])
	if !strings.Contains(message, "mask") {
		t.Errorf("error message = %q, should mention the mask", message)
	}
}

// mask 数量少于图片时，多出来的图回退到最后一个 mask（Python 的 masks[-1]）。
//
// 走真实路由：2 张图 + 1 个 mask，两张都必须带上同一个 alpha。
func TestImageEditSyncFallsBackToLastMask(t *testing.T) {
	server, upstream, _ := newMaskTestServer(t)

	status, body := postJSON(t, server, "/v1/images/edits", map[string]any{
		"prompt": "统一换成同一张 mask",
		"images": []any{
			solidPNGDataURL(t, 2, 1, color.NRGBA{R: 1, A: 255}),
			solidPNGDataURL(t, 2, 1, color.NRGBA{R: 2, A: 255}),
		},
		"mask": solidPNGDataURL(t, 2, 1, color.NRGBA{A: 0}),
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}

	images, _ := upstream.snapshot()
	if len(images) != 2 {
		t.Fatalf("images forwarded = %d, want 2", len(images))
	}
	for index := range images {
		if images[index][2] != "image/png" {
			t.Errorf("image %d mime = %q, want image/png", index, images[index][2])
		}
		if alpha := alphaOf(t, images[index], 0, 0); alpha != 0 {
			t.Errorf("image %d alpha = %d, want 0 (fallback mask)", index, alpha)
		}
	}
}

// 回归点：mask 合成失败也要进 call 日志。
//
// Python 用 LoggedCall 包住整个 handler，_composite_mask 抛异常会被记录成
// 「调用失败」。Go 版早期把 CompositeMask 放在 beginCall 之前，mask 坏掉时
// 请求根本不出现在日志页 —— 用户排查时看不到这次失败。
func TestImageEditMaskFailureIsLogged(t *testing.T) {
	server, _, deps := newMaskTestServer(t)

	status, body := postJSON(t, server, "/v1/images/edits", map[string]any{
		"prompt": "把左边换成一只猫",
		"image":  solidPNGDataURL(t, 4, 2, color.NRGBA{A: 255}),
		"mask":   "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not a png")),
	})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body = %s)", status, body)
	}

	logs := callLogs(deps)
	if len(logs) == 0 {
		t.Fatal("mask failure produced no call log entry; the request is invisible in the log page")
	}
	entry := logs[0]
	detail, ok := entry["detail"].(map[string]any)
	if !ok {
		t.Fatalf("log entry has no detail: %v", entry)
	}
	if got := stringValueOfTest(detail["status"]); got != "failed" {
		t.Errorf("log status = %q, want failed", got)
	}
	if got := stringValueOfTest(detail["endpoint"]); got != "/v1/images/edits" {
		t.Errorf("log endpoint = %q, want /v1/images/edits", got)
	}
	if got := stringValueOfTest(detail["error"]); !strings.Contains(got, "mask") {
		t.Errorf("log error = %q, should mention the mask", got)
	}
}

// 成功路径只写一条日志：失败路径自己 finish 之后，defer 不能重复写。
func TestImageEditSuccessLogsExactlyOnce(t *testing.T) {
	server, _, deps := newMaskTestServer(t)

	status, body := postJSON(t, server, "/v1/images/edits", map[string]any{
		"prompt": "把左边换成一只猫",
		"image":  solidPNGDataURL(t, 4, 2, color.NRGBA{A: 255}),
		"mask":   halfMaskDataURL(t, 4, 2),
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}

	if logs := callLogs(deps); len(logs) != 1 {
		t.Fatalf("call log entries = %d, want exactly 1", len(logs))
	}
}

// 超大图（解压炸弹）必须被拦在解码前，返回 502 而不是把进程打挂。
//
// 走真实路由，验证「不 OOM」这条在 handler 层也成立。
func TestImageEditRejectsOversizedImage(t *testing.T) {
	server, upstream, deps := newMaskTestServer(t)

	status, body := postJSON(t, server, "/v1/images/edits", map[string]any{
		"prompt": "把左边换成一只猫",
		"image":  oversizedPNGDataURL(t, 40000, 40000),
		"mask":   halfMaskDataURL(t, 2, 2),
	})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body = %s)", status, body)
	}
	if _, calls := upstream.snapshot(); calls != 0 {
		t.Errorf("upstream calls = %d, want 0", calls)
	}
	if logs := callLogs(deps); len(logs) != 1 {
		t.Errorf("call log entries = %d, want 1", len(logs))
	}
}

// oversizedPNGDataURL 造一个 IHDR 声明超大尺寸、IDAT 只有几十字节的 PNG data URL。
// 不真的分配像素缓冲 —— DecodeConfig 会先读到假尺寸，解码前就被拦下。
func oversizedPNGDataURL(t *testing.T, width, height int) string {
	t.Helper()
	header := make([]byte, 13)
	binary.BigEndian.PutUint32(header[0:], uint32(width))
	binary.BigEndian.PutUint32(header[4:], uint32(height))
	header[8] = 8 // bit depth
	header[9] = 0 // color type 0 = grayscale

	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	writeTestPNGChunk(&out, "IHDR", header)
	writeTestPNGChunk(&out, "IDAT", zlibBytes(t, make([]byte, 32)))
	writeTestPNGChunk(&out, "IEND", nil)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(out.Bytes())
}

func writeTestPNGChunk(out *bytes.Buffer, kind string, data []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	out.Write(length[:])
	payload := append([]byte(kind), data...)
	out.Write(payload)
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], crc32.ChecksumIEEE(payload))
	out.Write(sum[:])
}

func zlibBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zlib.NewWriter(&buf)
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}
