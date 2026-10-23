package service

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/basketikun/chatgpt2api/internal/config"
)

// image_storage.mode 分流（D3-I17）：local / webdav / both / 非法值。
//
// 回归点：Go 侧曾完全忽略 mode，永远只写本地、索引硬写 webdav:false，
// 于是设成 webdav 的用户本地磁盘照样涨满，远端一张也没有。

// webdavRecorder 在 webdavFake 外面套一层计数与故障注入。
type webdavRecorder struct {
	fake *webdavFake

	mu          sync.Mutex
	puts        int
	gets        int
	putStatus   int    // 非 0 时 PUT 直接返回该状态码（模拟远端 5xx）
	contentType string // 最后一次 PUT 的 Content-Type
}

func newWebDAVRecorder() *webdavRecorder {
	return &webdavRecorder{fake: newWebDAVFake()}
}

func (r *webdavRecorder) handler() http.Handler {
	inner := r.fake.handler()
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		switch req.Method {
		case http.MethodPut:
			r.puts++
			r.contentType = req.Header.Get("Content-Type")
			if r.putStatus != 0 {
				status := r.putStatus
				r.mu.Unlock()
				w.WriteHeader(status)
				return
			}
		case http.MethodGet:
			r.gets++
		}
		r.mu.Unlock()
		inner.ServeHTTP(w, req)
	})
}

func (r *webdavRecorder) lastContentType() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.contentType
}

func (r *webdavRecorder) counts() (puts, gets int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.puts, r.gets
}

// remoteFiles 返回远端已存文件的路径快照。
func (r *webdavRecorder) remoteFiles() map[string][]byte {
	r.fake.mu.Lock()
	defer r.fake.mu.Unlock()
	out := make(map[string][]byte, len(r.fake.files))
	for path, payload := range r.fake.files {
		out[path] = payload
	}
	return out
}

// remoteFile 找远端某个 rel 对应的内容（路径里含 root path 前缀）。
func (r *webdavRecorder) remoteFile(rel string) ([]byte, bool) {
	for path, payload := range r.remoteFiles() {
		if strings.HasSuffix(path, "/"+rel) {
			return payload, true
		}
	}
	return nil, false
}

// newImageStorageFixture 建一个带 WebDAV 配置的 ImageService。
// enabled 恒为 true —— 归一化里 enabled=false 会把 mode 强制回 local。
func newImageStorageFixture(t *testing.T, mode, webdavURL string) (*ImageService, *config.Store) {
	t.Helper()
	dir := t.TempDir()
	raw := map[string]any{
		"auth-key": "test",
		"image_storage": map[string]any{
			"enabled":          true,
			"mode":             mode,
			"webdav_url":       webdavURL,
			"webdav_username":  "",
			"webdav_password":  "secret",
			"webdav_root_path": "chatgpt2api/images",
			"public_base_url":  "",
		},
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), payload, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("2.0.0"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	t.Setenv("CHATGPT2API_AUTH_KEY", "")
	cfg, err := config.NewStore(dir)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return NewImageService(cfg), cfg
}

// testPNG 生成一张 w×h 的合法 PNG（宽高要能被 imageDimensionsFromBytes 解出来）。
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 20), G: uint8(y * 20), B: 128, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buffer.Bytes()
}

// indexItem 读回索引里某个 rel 的条目（原始 JSON，便于断言 storage 三态）。
func indexItem(t *testing.T, cfg *config.Store, rel string) (map[string]any, bool) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(cfg.DataDir(), "image_index.json"))
	if err != nil {
		return nil, false
	}
	var wrapper struct {
		Items map[string]map[string]any `json:"items"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		t.Fatalf("parse index: %v", err)
	}
	item, ok := wrapper.Items[rel]
	return item, ok
}

// localImageExists 判断本地是否真的落了盘（rel 为空表示 save 没返回路径）。
func localImageExists(t *testing.T, svc *ImageService, rel string) bool {
	t.Helper()
	if rel == "" {
		return false
	}
	full, err := svc.localImagePath(rel)
	if err != nil {
		t.Fatalf("localImagePath: %v", err)
	}
	_, statErr := os.Stat(full)
	return statErr == nil
}

// indexAll 返回整个索引。
func indexAll(t *testing.T, cfg *config.Store) map[string]map[string]any {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(cfg.DataDir(), "image_index.json"))
	if err != nil {
		return map[string]map[string]any{}
	}
	var wrapper struct {
		Items map[string]map[string]any `json:"items"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		t.Fatalf("parse index: %v", err)
	}
	return wrapper.Items
}

// localImageFiles 列出 images 目录下所有图片的相对路径。
func localImageFiles(t *testing.T, cfg *config.Store) []string {
	t.Helper()
	root := cfg.ImagesDir()
	out := make([]string, 0)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !isImageRel(info.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out
}

// numberOf 把索引里 JSON 还原出来的数字转成 int（float64/int 都认）。
func numberOf(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	default:
		return -1
	}
}

// local 模式：只写本地，绝不碰 WebDAV。
func TestSaveImageBytesLocalModeSkipsWebDAV(t *testing.T) {
	recorder := newWebDAVRecorder()
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, cfg := newImageStorageFixture(t, "local", server.URL)
	rel, url, err := svc.SaveImageBytes(testPNG(t, 4, 3), "http://host")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if url != "http://host/images/"+rel {
		t.Errorf("url = %q", url)
	}

	puts, gets := recorder.counts()
	if puts != 0 || gets != 0 {
		t.Errorf("local mode must not touch WebDAV: puts=%d gets=%d", puts, gets)
	}
	if !localImageExists(t, svc, rel) {
		t.Error("local file missing")
	}

	item, ok := indexItem(t, cfg, rel)
	if !ok {
		t.Fatalf("index entry missing for %q", rel)
	}
	if item["storage"] != "local" {
		t.Errorf("storage = %v, want local", item["storage"])
	}
	if item["local"] != true || item["webdav"] != false {
		t.Errorf("local/webdav = %v/%v, want true/false", item["local"], item["webdav"])
	}
	if item["remote_url"] != "" {
		t.Errorf("remote_url = %v, want empty", item["remote_url"])
	}
}

// webdav 模式：只传远端，本地一个字节都不写。
func TestSaveImageBytesWebDAVModeWritesRemoteOnly(t *testing.T) {
	recorder := newWebDAVRecorder()
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, cfg := newImageStorageFixture(t, "webdav", server.URL)
	payload := testPNG(t, 5, 2)
	rel, _, err := svc.SaveImageBytes(payload, "http://host")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	if localImageExists(t, svc, rel) {
		t.Error("webdav mode must not write local files")
	}
	puts, _ := recorder.counts()
	if puts != 1 {
		t.Errorf("puts = %d, want 1", puts)
	}
	remote, ok := recorder.remoteFile(rel)
	if !ok {
		t.Fatalf("remote file missing for %q", rel)
	}
	if !bytes.Equal(remote, payload) {
		t.Error("remote payload differs from saved bytes")
	}
	// 图片按 .png 落盘，PUT 要带 image/png（Python 的 content_type 参数）。
	if contentType := recorder.lastContentType(); contentType != "image/png" {
		t.Errorf("PUT Content-Type = %q, want image/png", contentType)
	}

	item, ok := indexItem(t, cfg, rel)
	if !ok {
		t.Fatalf("index entry missing for %q", rel)
	}
	if item["storage"] != "webdav" {
		t.Errorf("storage = %v, want webdav", item["storage"])
	}
	if item["local"] != false || item["webdav"] != true {
		t.Errorf("local/webdav = %v/%v, want false/true", item["local"], item["webdav"])
	}
	if remoteURL := stringValue(item["remote_url"]); remoteURL == "" || !strings.HasSuffix(remoteURL, rel) {
		t.Errorf("remote_url = %v, want WebDAV url ending with %q", item["remote_url"], rel)
	}
	// 宽高要从字节解（本地没有文件可读）。
	if numberOf(item["width"]) != 5 || numberOf(item["height"]) != 2 {
		t.Errorf("width/height = %v/%v, want 5/2", item["width"], item["height"])
	}
}

// both 模式：两边都写，索引 storage=both。
func TestSaveImageBytesBothMode(t *testing.T) {
	recorder := newWebDAVRecorder()
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, cfg := newImageStorageFixture(t, "both", server.URL)
	payload := testPNG(t, 3, 3)
	rel, _, err := svc.SaveImageBytes(payload, "http://host")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	if !localImageExists(t, svc, rel) {
		t.Error("both mode must write local file")
	}
	if remote, ok := recorder.remoteFile(rel); !ok || !bytes.Equal(remote, payload) {
		t.Errorf("both mode must upload payload, got ok=%v", ok)
	}

	item, ok := indexItem(t, cfg, rel)
	if !ok {
		t.Fatalf("index entry missing for %q", rel)
	}
	if item["storage"] != "both" {
		t.Errorf("storage = %v, want both", item["storage"])
	}
	if item["local"] != true || item["webdav"] != true {
		t.Errorf("local/webdav = %v/%v, want true/true", item["local"], item["webdav"])
	}
}

// get_bytes：本地缺失且索引 webdav:true → 回退远端读取；本地优先。
func TestReadImageFallsBackToWebDAV(t *testing.T) {
	recorder := newWebDAVRecorder()
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, _ := newImageStorageFixture(t, "webdav", server.URL)
	payload := testPNG(t, 2, 2)
	rel, _, err := svc.SaveImageBytes(payload, "http://host")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	got, mime, err := svc.ReadImage(rel)
	if err != nil {
		t.Fatalf("ReadImage fallback: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("remote payload differs")
	}
	if mime != "image/png" {
		t.Errorf("mime = %q, want image/png", mime)
	}
}

// 本地文件存在时优先读本地，不去远端。
func TestReadImagePrefersLocalFile(t *testing.T) {
	recorder := newWebDAVRecorder()
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, _ := newImageStorageFixture(t, "both", server.URL)
	rel, _, err := svc.SaveImageBytes(testPNG(t, 2, 2), "http://host")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	full, err := svc.localImagePath(rel)
	if err != nil {
		t.Fatalf("localImagePath: %v", err)
	}
	local := []byte("LOCAL-OVERRIDE")
	if err := os.WriteFile(full, local, 0o644); err != nil {
		t.Fatalf("overwrite local: %v", err)
	}

	got, _, err := svc.ReadImage(rel)
	if err != nil {
		t.Fatalf("ReadImage: %v", err)
	}
	if !bytes.Equal(got, local) {
		t.Error("ReadImage must prefer the local file")
	}
	_, gets := recorder.counts()
	if gets != 0 {
		t.Errorf("local hit must not GET remote, gets=%d", gets)
	}
}

// 索引没有 webdav:true 时，本地缺失就是 404，不能去远端猜。
func TestReadImageMissingWithoutWebDAVFlag(t *testing.T) {
	recorder := newWebDAVRecorder()
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, _ := newImageStorageFixture(t, "local", server.URL)
	rel, _, err := svc.SaveImageBytes(testPNG(t, 2, 2), "http://host")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	full, err := svc.localImagePath(rel)
	if err != nil {
		t.Fatalf("localImagePath: %v", err)
	}
	if err := os.Remove(full); err != nil {
		t.Fatalf("remove local: %v", err)
	}

	if _, _, err := svc.ReadImage(rel); err == nil {
		t.Fatal("missing local file without webdav flag must 404")
	}
	_, gets := recorder.counts()
	if gets != 0 {
		t.Errorf("must not GET remote when index lacks webdav flag, gets=%d", gets)
	}
}

// WebDAV 失败必须让 save 失败（不能静默只存本地），错误里带 HTTP 状态。
func TestSaveImageBytesWebDAVFailureFailsSave(t *testing.T) {
	for _, mode := range []string{"webdav", "both"} {
		t.Run(mode, func(t *testing.T) {
			recorder := newWebDAVRecorder()
			recorder.putStatus = http.StatusInternalServerError
			server := httptest.NewServer(recorder.handler())
			defer server.Close()

			svc, cfg := newImageStorageFixture(t, mode, server.URL)
			rel, url, err := svc.SaveImageBytes(testPNG(t, 2, 2), "http://host")
			if err == nil {
				t.Fatalf("save must fail when WebDAV returns 500 (rel=%q url=%q)", rel, url)
			}
			if !strings.Contains(err.Error(), "HTTP 500") {
				t.Errorf("error must mention HTTP status, got: %v", err)
			}

			// 索引不能留下「已存好」的条目：否则列表里会出现一张打不开的图。
			if items := indexAll(t, cfg); len(items) != 0 {
				t.Errorf("index must stay empty after a failed save, got %v", items)
			}
			// webdav 模式失败时绝不能退回本地（那正是这个 bug 的核心）。
			if mode == "webdav" {
				if files := localImageFiles(t, cfg); len(files) != 0 {
					t.Errorf("webdav mode must not fall back to local on failure, got %v", files)
				}
			}
		})
	}
}

// 非法 mode 一律按 local 处理（Python save() 里的白名单校验）。
func TestImageStorageModeFallsBackToLocal(t *testing.T) {
	for _, mode := range []string{"", "  ", "remote", "WEBDAV ", "s3"} {
		if got := imageStorageMode(map[string]any{"mode": mode}); got != "local" {
			t.Errorf("mode %q → %q, want local", mode, got)
		}
	}
	for _, mode := range []string{"local", "webdav", "both"} {
		if got := imageStorageMode(map[string]any{"mode": mode}); got != mode {
			t.Errorf("mode %q → %q, want unchanged", mode, got)
		}
	}
	if got := imageStorageMode(nil); got != "local" {
		t.Errorf("nil settings → %q, want local", got)
	}
}

// zipEntryNames 列出 zip 里的条目名。
func zipEntryNames(t *testing.T, payload []byte) []string {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	names := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		names = append(names, file.Name)
	}
	return names
}

// zip 下载同样走 get_bytes 的回退：webdav 模式下本地无文件也能打包。
func TestDownloadZipFallsBackToWebDAV(t *testing.T) {
	recorder := newWebDAVRecorder()
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, _ := newImageStorageFixture(t, "webdav", server.URL)
	payload := testPNG(t, 2, 2)
	rel, _, err := svc.SaveImageBytes(payload, "http://host")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	archive, err := svc.DownloadZip([]string{rel})
	if err != nil {
		t.Fatalf("DownloadZip: %v", err)
	}
	if len(archive) == 0 {
		t.Fatal("empty zip")
	}
	// zip 里应有且仅有这一张图（名字是 rel 的 basename）。
	names := zipEntryNames(t, archive)
	want := filepath.Base(rel)
	if len(names) != 1 || names[0] != want {
		t.Errorf("zip entries = %v, want [%q]", names, want)
	}
}

// webdav 模式下缩略图要能生成（源图只在远端）。
func TestEnsureThumbnailFromWebDAVSource(t *testing.T) {
	recorder := newWebDAVRecorder()
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, _ := newImageStorageFixture(t, "webdav", server.URL)
	rel, _, err := svc.SaveImageBytes(testPNG(t, 64, 48), "http://host")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	thumb, err := svc.EnsureThumbnail(rel)
	if err != nil {
		t.Fatalf("EnsureThumbnail: %v", err)
	}
	if !fileExists(thumb) {
		t.Fatalf("thumbnail missing at %q", thumb)
	}
	width, height, ok := imageDimensions(thumb)
	if !ok {
		t.Fatal("thumbnail is not a decodable image")
	}
	if width != 64 || height != 48 {
		t.Errorf("thumbnail size = %dx%d, want 64x48 (no upscale)", width, height)
	}
	_, gets := recorder.counts()
	if gets != 1 {
		t.Errorf("thumbnail should GET the remote image once, gets=%d", gets)
	}
}

// 删除 webdav 模式的图要连远端一起删（否则远端留一份删不掉的孤儿）。
func TestDeleteImagesRemovesRemoteCopy(t *testing.T) {
	recorder := newWebDAVRecorder()
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, cfg := newImageStorageFixture(t, "webdav", server.URL)
	rel, _, err := svc.SaveImageBytes(testPNG(t, 2, 2), "http://host")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, ok := recorder.remoteFile(rel); !ok {
		t.Fatalf("remote copy missing before delete (%q)", rel)
	}

	result := svc.DeleteImages([]string{rel}, "", "", false)
	if removed := numberOf(result["removed"]); removed != 1 {
		t.Errorf("removed = %v, want 1", result["removed"])
	}
	if _, ok := recorder.remoteFile(rel); ok {
		t.Errorf("remote copy must be deleted for %q", rel)
	}
	if items := indexAll(t, cfg); len(items) != 0 {
		t.Errorf("index entry must be removed, got %v", items)
	}
}

// local 模式的图删除时不能碰 WebDAV（索引没标 webdav）。
func TestDeleteImagesLocalOnlySkipsWebDAV(t *testing.T) {
	recorder := newWebDAVRecorder()
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, _ := newImageStorageFixture(t, "local", server.URL)
	rel, _, err := svc.SaveImageBytes(testPNG(t, 2, 2), "http://host")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	svc.DeleteImages([]string{rel}, "", "", false)
	if _, ok := recorder.remoteFile(rel); ok {
		t.Error("local mode must not upload anything")
	}
	if localImageExists(t, svc, rel) {
		t.Error("local file should be removed")
	}
}
