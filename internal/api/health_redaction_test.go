package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/api"
)

// /health?format=json 是**未鉴权**端点，不能泄漏服务器绝对路径。
//
// 实测：它返回 storage.backend.file_path = /home/chatgpt2api/data/accounts.json
// 与 auth_keys_file_path。这两条信息对攻击者直接有用（定位部署目录、
// 判断是否 Docker、推断宿主机用户名），而健康检查完全不需要它们。
//
// 号池规模（total/active/disabled）是运营指标，Python 版就公开，保留。
func TestHealthJSONDoesNotLeakAbsolutePaths(t *testing.T) {
	deps := newTestDeps(t)
	router := api.NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/health?format=json", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	// 任何形如 /home/... /app/... 的绝对路径都不该出现。
	for _, needle := range []string{"/home/", "/app/", "/tmp/", "/root/"} {
		if strings.Contains(body, needle) {
			t.Errorf("未鉴权的 /health 泄漏绝对路径片段 %q:\n%s", needle, body)
		}
	}

	// 但结构要保留（不能因为脱敏把整个 storage 段删掉，破坏契约）。
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := payload["storage"]; !ok {
		t.Error("storage 段被整体删除，契约被破坏")
	}
	if _, ok := payload["accounts"]; !ok {
		t.Error("accounts 段丢失")
	}
}
