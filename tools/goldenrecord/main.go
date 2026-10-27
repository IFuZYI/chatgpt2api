// Command goldenrecord 从运行中的 Python 服务录制 golden 快照。
//
// 用法：
//
//	go run ./tools/goldenrecord -base http://127.0.0.1:8012 -out testdata/golden
//
// 它只读端点，不写任何状态；录到的文件被 internal/api/golden_test.go 用作
// Go 版行为等价的契约（见 docs/adr/0003-golden-baseline-equivalence.md）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Compare 决定 Go 版与快照的比对严格度。
//
// 实测发现「逐字节比对」是错误的判据：
//   - JSON 对象的键顺序不是契约的一部分。Python 保留 dict 插入顺序，
//     Go 的 map 序列化按键名排序，两者语义等价但字节不同。
//   - 部分端点返回环境相关数据（绝对路径、磁盘容量、日志内容），
//     在测试环境必然不同。
//
// 所以分三档：exact（HTML/纯文本，字节相等）、json（语义深比较）、
// shape（只比状态码、content-type 与顶层键集合）。
type Compare string

const (
	CompareExact Compare = "exact"
	CompareJSON  Compare = "json"
	CompareShape Compare = "shape"
)

// Case 是一个 golden 用例。
type Case struct {
	Name    string
	Method  string
	Path    string
	Headers map[string]string
	Body    string
	Compare Compare
	// Pending 表示该端点尚未在 Go 版实现；测试会 skip 并打印原因。
	Pending string
}

// Golden 是落盘结构。
//
// 请求部分（RequestHeaders/RequestBody）必须一起录，否则回放时无法复现
// 「带不带 Authorization」这类决定状态码的差异——这是第一次录制踩到的坑。
// 响应头只保留 content-type，因为 Date/Server 之类每次都会变。
type Golden struct {
	Name        string `json:"name"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`

	RequestHeaders map[string]string `json:"request_headers"`
	RequestBody    string            `json:"request_body"`

	Compare Compare `json:"compare"`

	// Pending 非空表示该端点尚未实现（M3/M4/M5 补齐后置空）。
	Pending string `json:"pending,omitempty"`
	// IntentionalDivergence 标记「Go 版刻意不同」的用例，附原因。
	IntentionalDivergence string `json:"intentional_divergence,omitempty"`
}

func main() {
	base := flag.String("base", "http://127.0.0.1:8012", "base url of the python service")
	out := flag.String("out", "testdata/golden", "output directory")
	authKey := flag.String("auth-key", "", "auth key; empty means read config.json")
	flag.Parse()

	key := *authKey
	if key == "" {
		payload, err := os.ReadFile("config.json")
		if err == nil {
			var cfg map[string]any
			if json.Unmarshal(payload, &cfg) == nil {
				if text, ok := cfg["auth-key"].(string); ok {
					key = text
				}
			}
		}
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "auth key not found; pass -auth-key")
		os.Exit(1)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		os.Exit(1)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	failures := 0
	for _, tc := range cases(key) {
		golden, err := record(client, *base, tc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%-46s ERROR %v\n", tc.Name, err)
			failures++
			continue
		}
		payload, err := json.MarshalIndent(golden, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "%-46s marshal %v\n", tc.Name, err)
			failures++
			continue
		}
		path := filepath.Join(*out, tc.Name+".json")
		if err := os.WriteFile(path, append(payload, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "%-46s write %v\n", tc.Name, err)
			failures++
			continue
		}
		fmt.Printf("%-46s %d\n", tc.Name, golden.Status)
	}
	fmt.Printf("\nrecorded into %s (%d failures)\n", *out, failures)
	if failures > 0 {
		os.Exit(1)
	}
}

func record(client *http.Client, base string, tc Case) (Golden, error) {
	var reader io.Reader
	if tc.Body != "" {
		reader = strings.NewReader(tc.Body)
	}
	req, err := http.NewRequest(tc.Method, strings.TrimRight(base, "/")+tc.Path, reader)
	if err != nil {
		return Golden{}, err
	}
	for name, value := range tc.Headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Golden{}, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return Golden{}, err
	}

	body := string(payload)
	contentType := resp.Header.Get("Content-Type")
	if index := strings.IndexByte(contentType, ';'); index >= 0 {
		contentType = contentType[:index]
	}
	return Golden{
		Name:           tc.Name,
		Method:         tc.Method,
		Path:           tc.Path,
		Status:         resp.StatusCode,
		ContentType:    strings.TrimSpace(contentType),
		Body:           body,
		RequestHeaders: tc.Headers,
		RequestBody:    tc.Body,
		Compare:        compareOf(tc),
		Pending:        tc.Pending,
	}, nil
}

func compareOf(tc Case) Compare {
	if tc.Compare != "" {
		return tc.Compare
	}
	return CompareJSON
}

func cases(authKey string) []Case {
	admin := map[string]string{"Authorization": "Bearer " + authKey}
	jsonAdmin := map[string]string{
		"Authorization": "Bearer " + authKey,
		"Content-Type":  "application/json",
	}
	badAuth := map[string]string{"Authorization": "Bearer definitely-wrong"}
	anonJSON := map[string]string{"Content-Type": "application/json"}

	const pendingM3 = "协议层（/v1/*）在 M3 实现"
	const pendingM5 = "账号写入链路在 M5 实现"

	list := []Case{
		// ---- 基础：HTML 逐字节、JSON 语义比较 ----
		{Name: "version", Method: "GET", Path: "/version", Compare: CompareJSON},
		{Name: "health_html", Method: "GET", Path: "/health", Compare: CompareExact},
		// 健康 JSON 含绝对路径与磁盘容量，跨环境必然不同 → 只比结构。
		{Name: "health_json", Method: "GET", Path: "/health?format=json", Compare: CompareShape},

		// ---- 三套信封：管理面 ----
		{Name: "admin_401_accounts", Method: "GET", Path: "/api/accounts", Compare: CompareJSON},
		{Name: "admin_401_settings", Method: "GET", Path: "/api/settings", Compare: CompareJSON},
		{Name: "admin_401_bad_key", Method: "GET", Path: "/api/accounts", Headers: badAuth, Compare: CompareJSON},
		{Name: "admin_404", Method: "GET", Path: "/api/does-not-exist", Compare: CompareJSON},
		{Name: "admin_405", Method: "PUT", Path: "/api/accounts", Compare: CompareJSON},

		// ---- 三套信封：OpenAI ----
		{Name: "openai_401_models", Method: "GET", Path: "/v1/models", Compare: CompareJSON, Pending: pendingM3},
		{Name: "openai_401_chat", Method: "POST", Path: "/v1/chat/completions",
			Headers: jsonAdmin, Body: `{}`, Compare: CompareJSON, Pending: pendingM3},
		{Name: "openai_404_unknown", Method: "GET", Path: "/v1/does-not-exist", Compare: CompareJSON},
		{Name: "openai_405_method", Method: "DELETE", Path: "/v1/models", Compare: CompareJSON, Pending: pendingM3},

		// ---- 三套信封：Anthropic ----
		{Name: "anthropic_401_messages", Method: "POST", Path: "/v1/messages",
			Headers: anonJSON, Body: `{}`, Compare: CompareJSON, Pending: pendingM3},

		// ---- 参数校验 ----
		{Name: "admin_400_accounts_empty", Method: "POST", Path: "/api/accounts",
			Headers: jsonAdmin, Body: `{}`, Compare: CompareJSON, Pending: pendingM5},
		{Name: "admin_422_accounts_tokens_not_list", Method: "POST", Path: "/api/accounts",
			Headers: jsonAdmin, Body: `{"tokens":"nope"}`, Compare: CompareJSON},
		{Name: "openai_400_chat_no_messages", Method: "POST", Path: "/v1/chat/completions",
			Headers: jsonAdmin, Body: `{}`, Compare: CompareJSON, Pending: pendingM3},

		// ---- 设置 ----
		{Name: "admin_settings", Method: "GET", Path: "/api/settings", Headers: admin, Compare: CompareJSON},
		// `/api/third-party-apps` **刻意不录**：Go 版有意删除该端点
		// （Python 78 条路由 → 77 条），从 Python 录会得到 200、
		// Go 返回 404 —— 这个用例永远无法对等，登记它只会让人以为有覆盖。
		// 路由数量由 TestRouteParity 守卫，见 docs/archive-python-backend.md。

		// ---- 账号与密钥（空号池）----
		{Name: "admin_accounts_empty", Method: "GET", Path: "/api/accounts", Headers: admin, Compare: CompareJSON},
		{Name: "admin_auth_users", Method: "GET", Path: "/api/auth/users", Headers: admin, Compare: CompareJSON},

		// ---- 代理 ----
		{Name: "admin_proxy_runtime", Method: "GET", Path: "/api/proxy/runtime", Headers: admin, Compare: CompareJSON},
		// 前端有调用但后端从未定义过，两端都应是 404（见计划书修正 1）。
		{Name: "admin_proxy_missing", Method: "GET", Path: "/api/proxy", Headers: admin, Compare: CompareJSON},

		// ---- 图片 ----
		{Name: "admin_images", Method: "GET", Path: "/api/images", Headers: admin, Compare: CompareJSON},
		// 磁盘容量是环境数据 → 只比键集合。
		{Name: "admin_image_storage_stats", Method: "GET", Path: "/api/images/storage", Headers: admin, Compare: CompareShape},
		{Name: "admin_images_tags", Method: "GET", Path: "/api/images/tags", Headers: admin, Compare: CompareJSON},
		{Name: "admin_image_tasks", Method: "GET", Path: "/api/image-tasks", Headers: admin, Compare: CompareJSON},

		// ---- 日志（内容随运行变化）----
		{Name: "admin_logs", Method: "GET", Path: "/api/logs", Headers: admin, Compare: CompareShape},

		// ---- SPA ----
		{Name: "spa_next_asset", Method: "GET", Path: "/_next/static/missing.js", Compare: CompareJSON},
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}
