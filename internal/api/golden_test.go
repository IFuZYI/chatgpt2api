package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/basketikun/chatgpt2api/internal/api"
	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/basketikun/chatgpt2api/internal/storage"
)

// 比对严格度，与 tools/goldenrecord 的 Compare 一一对应。
const (
	compareExact = "exact"
	compareJSON  = "json"
	compareShape = "shape"
)

type golden struct {
	Name           string            `json:"name"`
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	Status         int               `json:"status"`
	ContentType    string            `json:"content_type"`
	Body           string            `json:"body"`
	RequestHeaders map[string]string `json:"request_headers"`
	RequestBody    string            `json:"request_body"`
	Compare        string            `json:"compare"`
	Pending        string            `json:"pending"`
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()

	// 用固定的 golden 配置，不用仓库的 config.json。
	//
	// 理由：config.json 是用户可改的运行时文件（比如加代理、改额度上限），
	// 拿它当测试输入会让 golden 随环境漂移 —— 实测踩到：加了全局代理后
	// admin_settings / admin_proxy_runtime 两个用例就红了。
	repoConfig, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "config.json"))
	if err != nil {
		t.Fatalf("read golden config.json: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(repoConfig, &raw); err != nil {
		t.Fatalf("parse golden config.json: %v", err)
	}
	raw["auth-key"] = "chatgpt2api"
	encoded, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), encoded, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("1.8.0\n"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}

	t.Setenv("CHATGPT2API_AUTH_KEY", "")
	t.Setenv("STORAGE_BACKEND", "json")
	t.Setenv("CHATGPT2API_WEB_DIST", "")

	cfg, err := config.NewStore(dir)
	if err != nil {
		t.Fatalf("config.NewStore: %v", err)
	}
	backend, err := storage.NewFromEnv(cfg.DataDir())
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	logs, err := service.NewLogService(filepath.Join(cfg.DataDir(), "logs.jsonl"))
	if err != nil {
		t.Fatalf("log service: %v", err)
	}
	deps := &api.Deps{
		Config:  cfg,
		Storage: backend,
		Logs:    logs,
		Auth:    service.NewAuthService(backend, cfg.AuthKey),
		Accts:   service.NewAccountService(backend, cfg, logs),
		Proxy:   service.NewProxySettings(cfg),
		Images:  service.NewImageService(cfg),
		Tasks:   service.NewImageTaskService(cfg),
		CPA:     service.NewCPAConfig(cfg.DataDir()),
		Sub2API: service.NewSub2APIConfig(cfg.DataDir()),
		OAuth:   service.NewOAuthLoginService(),
	}
	deps.Models = service.NewModelCatalog(deps.Accts, nil)
	deps.ChatCache = protocol.NewChatCompletionCache(deps.ChatCacheSettings)
	// 图片与文本上游在 golden 里不需要真实实现：所有用例都在鉴权/参数校验阶段返回。
	deps.TextUpstream = func(ctx context.Context, accessToken string) (protocol.Upstream, func(), error) {
		return nil, nil, fmt.Errorf("upstream not configured in golden test")
	}
	deps.ImageUpstream = func(ctx context.Context, accessToken string) (protocol.ImageUpstream, func(), error) {
		return nil, nil, fmt.Errorf("upstream not configured in golden test")
	}
	server := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(server.Close)
	return server
}

func loadGolden(t *testing.T) []golden {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "golden")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read golden dir: %v", err)
	}
	out := make([]golden, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		var item golden
		if err := json.Unmarshal(payload, &item); err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		t.Fatal("no golden files; run `go run ./tools/goldenrecord` first")
	}
	return out
}

// TestGoldenParity 是 ADR-0003 的核心断言。
//
// 三档比对：
//   - exact：HTML / 纯文本，字节相等
//   - json：JSON 语义深比较（键顺序不是契约）
//   - shape：只比状态码、content-type 与顶层键集合（环境相关数据）
func TestGoldenParity(t *testing.T) {
	server := newTestServer(t)
	client := server.Client()

	for _, want := range loadGolden(t) {
		want := want
		t.Run(want.Name, func(t *testing.T) {
			if want.Pending != "" {
				t.Skipf("pending: %s", want.Pending)
			}

			var body io.Reader
			if want.RequestBody != "" {
				body = strings.NewReader(want.RequestBody)
			}
			req, err := http.NewRequest(want.Method, server.URL+want.Path, body)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			for name, value := range want.RequestHeaders {
				req.Header.Set(name, value)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			payload, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}

			if resp.StatusCode != want.Status {
				t.Fatalf("status = %d, want %d (body=%s)", resp.StatusCode, want.Status, payload)
			}
			contentType := resp.Header.Get("Content-Type")
			if index := strings.IndexByte(contentType, ';'); index >= 0 {
				contentType = contentType[:index]
			}
			if strings.TrimSpace(contentType) != want.ContentType {
				t.Fatalf("content-type = %q, want %q", contentType, want.ContentType)
			}

			switch want.Compare {
			case compareExact:
				if string(payload) != want.Body {
					t.Fatalf("body mismatch\n got: %s\nwant: %s", payload, want.Body)
				}
			case compareShape:
				gotKeys := topLevelKeys(t, payload)
				wantKeys := topLevelKeys(t, []byte(want.Body))
				if !reflect.DeepEqual(gotKeys, wantKeys) {
					t.Fatalf("top-level keys = %v, want %v", gotKeys, wantKeys)
				}
			default:
				if !jsonEqual(t, payload, []byte(want.Body)) {
					t.Fatalf("json mismatch\n got: %s\nwant: %s", payload, want.Body)
				}
			}
		})
	}
}

// topLevelKeys 返回排序后的顶层键。非对象 body 返回 nil（空对象与非空对象都算空）。
func topLevelKeys(t *testing.T, payload []byte) []string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode json: %v (payload=%s)", err, payload)
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// jsonEqual 深比较两个 JSON 文档，忽略对象键顺序。
func jsonEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode got: %v (%s)", err, got)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode want: %v (%s)", err, want)
	}
	return reflect.DeepEqual(gotValue, wantValue)
}
