package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/api"
	"github.com/IFuZYI/chatgpt2api/internal/config"
	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/IFuZYI/chatgpt2api/internal/service"
)

// webdavStub 是最小 WebDAV 端点：MKCOL 恒成功，PUT 由 putStatus 决定。
type webdavStub struct {
	mu        sync.Mutex
	puts      int
	files     map[string][]byte
	putStatus int
}

func newWebDAVStub(putStatus int) *webdavStub {
	return &webdavStub{files: map[string][]byte{}, putStatus: putStatus}
}

func (s *webdavStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			s.puts++
			if s.putStatus != 0 {
				w.WriteHeader(s.putStatus)
				return
			}
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			s.files[r.URL.Path] = body
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			payload, ok := s.files[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(payload)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

func (s *webdavStub) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

// enableWebDAV 把 image_storage 切到指定 mode（enabled=true，否则归一化会强制回 local）。
func enableWebDAV(t *testing.T, cfg *config.Store, mode, url string) {
	t.Helper()
	if _, err := cfg.Update(map[string]any{
		"image_storage": map[string]any{
			"enabled":          true,
			"mode":             mode,
			"webdav_url":       url,
			"webdav_password":  "secret",
			"webdav_root_path": "chatgpt2api/images",
		},
	}); err != nil {
		t.Fatalf("update image_storage: %v", err)
	}
}

// newWebDAVRouteServer 建一个 codex 内联回图的服务器（落盘走真实的 SaveImageBytes）。
func newWebDAVRouteServer(t *testing.T, mode, webdavURL string) (*httptest.Server, *api.Deps) {
	t.Helper()
	deps := newTestDeps(t)
	deps.Accts.AddAccountItems([]map[string]any{
		{"access_token": "codex-token", "type": "plus", "source_type": "codex", "status": service.StatusNormal, "quota": 10},
	}, nil)

	upstream := &codexRouteUpstream{}
	deps.ImageUpstream = func(ctx context.Context, accessToken string) (protocol.ImageUpstream, func(), error) {
		return upstream, func() {}, nil
	}
	deps.RegisterImageTaskRunners()
	enableWebDAV(t, deps.Config, mode, webdavURL)

	server := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(server.Close)
	return server, deps
}

// imagesOnDisk 列出 data/images 下所有图片（webdav 模式必须为空）。
func imagesOnDisk(t *testing.T, deps *api.Deps) []string {
	t.Helper()
	root := deps.Config.ImagesDir()
	out := make([]string, 0)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.HasPrefix(info.Name(), ".") {
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

// indexItems 读回 image_index.json 的 items。
func indexItems(t *testing.T, deps *api.Deps) map[string]map[string]any {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(deps.Config.DataDir(), "image_index.json"))
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

// webdav 模式：200 + 索引 storage=webdav / local=false / webdav=true，本地目录空。
func TestGenerateStoresToWebDAVOnly(t *testing.T) {
	stub := newWebDAVStub(0)
	webdav := httptest.NewServer(stub.handler())
	defer webdav.Close()

	server, deps := newWebDAVRouteServer(t, "webdav", webdav.URL)
	status, body := postJSON(t, server, "/v1/images/generations", map[string]any{
		"prompt": "a cat",
		"model":  "codex-gpt-image-2",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if stub.putCount() != 1 {
		t.Errorf("webdav PUT count = %d, want 1", stub.putCount())
	}
	if files := imagesOnDisk(t, deps); len(files) != 0 {
		t.Errorf("webdav mode must not write local files, got %v", files)
	}

	items := indexItems(t, deps)
	if len(items) != 1 {
		t.Fatalf("index items = %v, want exactly 1", items)
	}
	for rel, item := range items {
		if item["storage"] != "webdav" {
			t.Errorf("%s storage = %v, want webdav", rel, item["storage"])
		}
		if item["local"] != false || item["webdav"] != true {
			t.Errorf("%s local/webdav = %v/%v, want false/true", rel, item["local"], item["webdav"])
		}
		if stringValueOfTest(item["remote_url"]) == "" {
			t.Errorf("%s remote_url must be set", rel)
		}
	}
}

// WebDAV 500 时接口必须返回 502，而不是 200 + 空 url。
//
// 回归点：FormatImageResult 曾用 `url, _ = saveImage(...)` 吞掉落盘错误，
// 于是 webdav 模式的用户在远端故障时仍看到「生成成功」，图却没存到任何地方。
func TestGenerateReturns502WhenWebDAVSaveFails(t *testing.T) {
	stub := newWebDAVStub(http.StatusInternalServerError)
	webdav := httptest.NewServer(stub.handler())
	defer webdav.Close()

	server, deps := newWebDAVRouteServer(t, "webdav", webdav.URL)
	status, body := postJSON(t, server, "/v1/images/generations", map[string]any{
		"prompt": "a cat",
		"model":  "codex-gpt-image-2",
	})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", status, body)
	}
	if !strings.Contains(string(body), "HTTP 500") {
		t.Errorf("error body should keep the WebDAV status: %s", body)
	}
	if files := imagesOnDisk(t, deps); len(files) != 0 {
		t.Errorf("webdav mode must not fall back to local, got %v", files)
	}
	if items := indexItems(t, deps); len(items) != 0 {
		t.Errorf("failed save must not be indexed, got %v", items)
	}
}

// both 模式：200，两边都有，索引 storage=both。
func TestGenerateStoresBothWhenModeIsBoth(t *testing.T) {
	stub := newWebDAVStub(0)
	webdav := httptest.NewServer(stub.handler())
	defer webdav.Close()

	server, deps := newWebDAVRouteServer(t, "both", webdav.URL)
	status, body := postJSON(t, server, "/v1/images/generations", map[string]any{
		"prompt": "a cat",
		"model":  "codex-gpt-image-2",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if files := imagesOnDisk(t, deps); len(files) != 1 {
		t.Errorf("both mode must write exactly one local file, got %v", files)
	}
	for rel, item := range indexItems(t, deps) {
		if item["storage"] != "both" {
			t.Errorf("%s storage = %v, want both", rel, item["storage"])
		}
	}
}

// webdav 模式下 /images/<rel> 能取到远端图（get_bytes 回退端到端）。
func TestServedImageFallsBackToWebDAV(t *testing.T) {
	stub := newWebDAVStub(0)
	webdav := httptest.NewServer(stub.handler())
	defer webdav.Close()

	server, deps := newWebDAVRouteServer(t, "webdav", webdav.URL)
	status, body := postJSON(t, server, "/v1/images/generations", map[string]any{
		"prompt": "a cat",
		"model":  "codex-gpt-image-2",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}

	var rel string
	for key := range indexItems(t, deps) {
		rel = key
	}
	if rel == "" {
		t.Fatal("no index entry after generate")
	}

	resp, err := server.Client().Get(server.URL + "/images/" + rel)
	if err != nil {
		t.Fatalf("GET /images: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /images/%s status = %d, want 200", rel, resp.StatusCode)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "image/") {
		t.Errorf("Content-Type = %q", contentType)
	}
}
