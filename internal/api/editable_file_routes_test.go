package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/api"
	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/IFuZYI/chatgpt2api/internal/service"
)

// fakeEditableUpstream 是 protocol.EditableFileUpstream 的测试替身。
//
// 它把「产物」直接写进 OutputDir 并回传路径 —— 上游链路的细节
// （上传/prepare/轮询/下载）由 internal/upstream 的测试覆盖，
// 这里只验证 api 层的接线：选号、内容过滤、URL 拼接、日志、404。
type fakeEditableUpstream struct {
	// mu 保护 requests：任务 goroutine 写入，测试在读。
	mu       sync.Mutex
	requests []protocol.EditableFileRequest
	// files 是文件名 → 内容；会写到 OutputDir。
	files map[string]string
	// err 非空时导出失败。
	err error
	// delay 模拟慢任务（测 Close 与任务状态用）。
	delay time.Duration
}

func (f *fakeEditableUpstream) ExportEditableFile(
	ctx context.Context,
	request protocol.EditableFileRequest,
) (protocol.EditableFileExportResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return protocol.EditableFileExportResult{}, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	if f.err != nil {
		return protocol.EditableFileExportResult{}, f.err
	}
	if err := os.MkdirAll(request.OutputDir, 0o755); err != nil {
		return protocol.EditableFileExportResult{}, err
	}
	result := protocol.EditableFileExportResult{ConversationID: "cid-fake"}
	for name, content := range f.files {
		target := filepath.Join(request.OutputDir, name)
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return protocol.EditableFileExportResult{}, err
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case ".zip":
			result.ZipPath = target
		default:
			result.PrimaryPath = target
		}
	}
	return result, nil
}

// calls 返回已记录请求的快照（并发安全）。
func (f *fakeEditableUpstream) calls() []protocol.EditableFileRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.EditableFileRequest{}, f.requests...)
}

// newEditableRouteServer 建一个注入了假可编辑文件上游的测试服务器。
func newEditableRouteServer(
	t *testing.T,
	accounts []map[string]any,
	upstream *fakeEditableUpstream,
) (*httptest.Server, *api.Deps) {
	t.Helper()
	deps := newTestDeps(t)
	if len(accounts) > 0 {
		deps.Accts.AddAccountItems(accounts, nil)
	}
	deps.EditableTasks = service.NewEditableFileTaskService(deps.Config.DataDir())
	deps.EditableUpstream = func(ctx context.Context, accessToken string) (protocol.EditableFileUpstream, func(), error) {
		return upstream, func() {}, nil
	}
	deps.RegisterEditableFileTaskRunner()
	// 任务 goroutine 会写 data/，测试结束前必须关掉（同 image task）。
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := deps.EditableTasks.Close(closeCtx); err != nil {
			t.Errorf("close editable tasks: %v", err)
		}
	})
	server := httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(server.Close)
	return server, deps
}

// getWithAuth 发一个带鉴权的 GET。
func getWithAuth(t *testing.T, server *httptest.Server, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
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

// editableAccount 是能跑可编辑文件的 plus 账号。
func editableAccount() map[string]any {
	return map[string]any{
		"access_token": "plus-token",
		"email":        "plus@example.com",
		"type":         "plus",
		"status":       service.StatusNormal,
	}
}

// waitEditableHTTPTask 轮询 /v1/editable-file-tasks 直到任务终态。
func waitEditableHTTPTask(t *testing.T, server *httptest.Server, taskID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, payload := getWithAuth(t, server, "/v1/editable-file-tasks?ids="+taskID)
		if status != http.StatusOK {
			t.Fatalf("list tasks status = %d, body = %s", status, payload)
		}
		var body struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatalf("decode tasks: %v (%s)", err, payload)
		}
		if len(body.Items) == 1 {
			status := stringValueOfTest(body.Items[0]["status"])
			if status == service.TaskStatusSuccess || status == service.TaskStatusError {
				return body.Items[0]
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach a terminal state", taskID)
	return nil
}

// PPT 提交：返回任务对象、异步跑完、结果里带两个 /files/ 链接，产物可下载。
func TestEditablePPTRouteCreatesTaskAndServesFiles(t *testing.T) {
	upstream := &fakeEditableUpstream{files: map[string]string{
		"deck.pptx":  "PPTX",
		"assets.zip": "ZIP",
	}}
	server, deps := newEditableRouteServer(t, []map[string]any{editableAccount()}, upstream)

	status, payload := postJSON(t, server, "/v1/ppt/generations", map[string]any{
		"prompt":         "做一份产品介绍",
		"client_task_id": "ppt-1",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	var submitted map[string]any
	if err := json.Unmarshal(payload, &submitted); err != nil {
		t.Fatalf("decode submit response: %v (%s)", err, payload)
	}
	if stringValueOfTest(submitted["id"]) != "ppt-1" || stringValueOfTest(submitted["taskId"]) != "ppt-1" {
		t.Errorf("submitted ids = %v / %v, want ppt-1", submitted["id"], submitted["taskId"])
	}
	if stringValueOfTest(submitted["kind"]) != "ppt" {
		t.Errorf("kind = %v, want ppt", submitted["kind"])
	}

	task := waitEditableHTTPTask(t, server, "ppt-1")
	if stringValueOfTest(task["status"]) != service.TaskStatusSuccess {
		t.Fatalf("task = %+v, want success", task)
	}
	result, _ := task["result"].(map[string]any)
	if stringValueOfTest(result["conversation_id"]) != "cid-fake" {
		t.Errorf("conversation_id = %v, want cid-fake", result["conversation_id"])
	}

	primaryURL := stringValueOfTest(result["primary_url"])
	zipURL := stringValueOfTest(result["zip_url"])
	// 链接前缀来自 resolve_image_base_url（配置的 base_url，否则 scheme://host），
	// 与 Python 的 _file_url 一致：{prefix}/files/{rel}。
	for _, url := range []string{primaryURL, zipURL} {
		if !strings.HasPrefix(url, server.URL+"/files/ppt/ppt-1/") {
			t.Errorf("artifact url = %q, want %s/files/ppt/ppt-1/...", url, server.URL)
		}
	}

	// 链接必须真的能下载（/files/* 不需要鉴权，与 Python 一致）。
	response, err := http.Get(primaryURL)
	if err != nil {
		t.Fatalf("GET primary: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != "PPTX" {
		t.Fatalf("primary download = %d %q, want 200 PPTX", response.StatusCode, string(body))
	}

	// 上游收到的请求：kind / prompt / base64_images 原样透传。
	calls := upstream.calls()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(calls))
	}
	if calls[0].Kind != protocol.EditableKindPPT || calls[0].Prompt != "做一份产品介绍" {
		t.Errorf("upstream request = %+v", calls[0])
	}
	if want := filepath.Join(deps.Config.DataDir(), "files", "ppt", "ppt-1"); calls[0].OutputDir != want {
		t.Errorf("output dir = %q, want %q", calls[0].OutputDir, want)
	}

	// 成功后必须记账（last_used_at 被写上）。
	account := deps.Accts.GetAccount("plus-token")
	if account == nil || stringValueOfTest(account["last_used_at"]) == "" {
		t.Errorf("account last_used_at = %v, want a timestamp", account["last_used_at"])
	}
}

// PSD 提交：kind=psd、参考图透传、产物落 data/files/psd/。
func TestEditablePSDRoutePassesImages(t *testing.T) {
	upstream := &fakeEditableUpstream{files: map[string]string{
		"poster.psd": "PSD",
		"layers.zip": "ZIP",
	}}
	server, _ := newEditableRouteServer(t, []map[string]any{editableAccount()}, upstream)

	status, payload := postJSON(t, server, "/v1/psd/generations", map[string]any{
		"prompt":         "拆图层",
		"client_task_id": "psd-1",
		"base64_images":  []any{"data:image/png;base64,AAAA"},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	task := waitEditableHTTPTask(t, server, "psd-1")
	if stringValueOfTest(task["status"]) != service.TaskStatusSuccess {
		t.Fatalf("task = %+v, want success", task)
	}
	if stringValueOfTest(task["kind"]) != "psd" {
		t.Errorf("kind = %v, want psd", task["kind"])
	}
	psdCalls := upstream.calls()
	if len(psdCalls) != 1 || psdCalls[0].Kind != protocol.EditableKindPSD {
		t.Fatalf("upstream requests = %+v, want one psd request", psdCalls)
	}
	if len(psdCalls[0].Base64Images) != 1 {
		t.Errorf("base64_images = %v, want 1 image", psdCalls[0].Base64Images)
	}
	if !strings.Contains(psdCalls[0].OutputDir, filepath.Join("files", "psd", "psd-1")) {
		t.Errorf("output dir = %q, want .../files/psd/psd-1", psdCalls[0].OutputDir)
	}
}

// PSD 无参考图：任务失败并带 base64_images is empty。
func TestEditablePSDRouteRequiresImages(t *testing.T) {
	upstream := &fakeEditableUpstream{files: map[string]string{"poster.psd": "PSD", "layers.zip": "ZIP"}}
	server, _ := newEditableRouteServer(t, []map[string]any{editableAccount()}, upstream)

	status, payload := postJSON(t, server, "/v1/psd/generations", map[string]any{
		"prompt":         "拆图层",
		"client_task_id": "psd-noimg",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}

	task := waitEditableHTTPTask(t, server, "psd-noimg")
	if stringValueOfTest(task["status"]) != service.TaskStatusError {
		t.Fatalf("status = %v, want error", task["status"])
	}
	if stringValueOfTest(task["error"]) != "base64_images is empty" {
		t.Errorf("error = %q, want base64_images is empty", stringValueOfTest(task["error"]))
	}
	if calls := upstream.calls(); len(calls) != 0 {
		t.Errorf("upstream must not be called, got %d calls", len(calls))
	}
}

// 没有可用账号：任务失败并带 Python 文案。
func TestEditableRouteWithoutAccountFails(t *testing.T) {
	upstream := &fakeEditableUpstream{files: map[string]string{"deck.pptx": "PPTX", "assets.zip": "ZIP"}}
	server, _ := newEditableRouteServer(t, []map[string]any{
		{"access_token": "free-token", "type": "free", "status": service.StatusNormal},
	}, upstream)

	status, payload := postJSON(t, server, "/v1/ppt/generations", map[string]any{
		"prompt":         "p",
		"client_task_id": "no-acct",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	task := waitEditableHTTPTask(t, server, "no-acct")
	if stringValueOfTest(task["error"]) != "no available plus/team/pro account" {
		t.Fatalf("error = %q, want no available plus/team/pro account", stringValueOfTest(task["error"]))
	}
	if calls := upstream.calls(); len(calls) != 0 {
		t.Errorf("upstream must not be called without an account, got %d calls", len(calls))
	}
}

// 上游失败：任务 status=error，错误文案透传。
func TestEditableRouteUpstreamFailure(t *testing.T) {
	upstream := &fakeEditableUpstream{err: errors.New("upstream exploded")}
	server, _ := newEditableRouteServer(t, []map[string]any{editableAccount()}, upstream)

	status, _ := postJSON(t, server, "/v1/ppt/generations", map[string]any{
		"prompt":         "p",
		"client_task_id": "boom",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (task queued)", status)
	}
	task := waitEditableHTTPTask(t, server, "boom")
	if stringValueOfTest(task["error"]) != "upstream exploded" {
		t.Fatalf("error = %q, want upstream exploded", stringValueOfTest(task["error"]))
	}
}

// 内容过滤：敏感词命中时 400，且不入队。
func TestEditableRouteContentFilterBlocks(t *testing.T) {
	upstream := &fakeEditableUpstream{files: map[string]string{"deck.pptx": "PPTX", "assets.zip": "ZIP"}}
	server, deps := newEditableRouteServer(t, []map[string]any{editableAccount()}, upstream)
	deps.ContentFilter = service.NewContentFilter(service.ContentFilterOptions{
		SensitiveWords: func() []string { return []string{"违禁词"} },
	})

	status, payload := postJSON(t, server, "/v1/ppt/generations", map[string]any{
		"prompt":         "这里包含违禁词",
		"client_task_id": "blocked",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", status, payload)
	}
	if !strings.Contains(string(payload), "敏感词") {
		t.Errorf("body = %s, want 敏感词 message", payload)
	}
	if calls := upstream.calls(); len(calls) != 0 {
		t.Errorf("upstream must not be called for blocked prompts, got %d calls", len(calls))
	}
	if task := deps.EditableTasks.GetTask("", "blocked"); task != nil {
		t.Errorf("blocked prompt must not be queued, got %+v", task)
	}
}

// 鉴权：没有 Bearer 时 401，且不入队。
func TestEditableRouteRequiresAuth(t *testing.T) {
	upstream := &fakeEditableUpstream{files: map[string]string{"deck.pptx": "PPTX", "assets.zip": "ZIP"}}
	server, _ := newEditableRouteServer(t, []map[string]any{editableAccount()}, upstream)

	for _, path := range []string{"/v1/ppt/generations", "/v1/psd/generations", "/v1/editable-file-tasks"} {
		method := http.MethodPost
		if strings.HasSuffix(path, "editable-file-tasks") {
			method = http.MethodGet
		}
		request, err := http.NewRequest(method, server.URL+path, bytes.NewReader([]byte(`{"prompt":"p"}`)))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", path, response.StatusCode)
		}
	}
}

// 幂等：同一 client_task_id 提交两次返回同一任务，上游只跑一次。
func TestEditableRouteSubmitIsIdempotent(t *testing.T) {
	upstream := &fakeEditableUpstream{files: map[string]string{"deck.pptx": "PPTX", "assets.zip": "ZIP"}}
	server, _ := newEditableRouteServer(t, []map[string]any{editableAccount()}, upstream)

	firstStatus, firstPayload := postJSON(t, server, "/v1/ppt/generations", map[string]any{
		"prompt": "p", "client_task_id": "dup",
	})
	if firstStatus != http.StatusOK {
		t.Fatalf("first status = %d", firstStatus)
	}
	secondStatus, secondPayload := postJSON(t, server, "/v1/ppt/generations", map[string]any{
		"prompt": "p", "client_task_id": "dup",
	})
	if secondStatus != http.StatusOK {
		t.Fatalf("second status = %d", secondStatus)
	}

	var first, second map[string]any
	_ = json.Unmarshal(firstPayload, &first)
	_ = json.Unmarshal(secondPayload, &second)
	if stringValueOfTest(first["id"]) != stringValueOfTest(second["id"]) {
		t.Fatalf("ids differ: %v vs %v", first["id"], second["id"])
	}
	waitEditableHTTPTask(t, server, "dup")
	if calls := upstream.calls(); len(calls) != 1 {
		t.Errorf("upstream calls = %d, want 1", len(calls))
	}
}

// /files/* 目录穿越必须 404（Python 用 relative_to 断言）。
func TestFilesRouteRejectsTraversal(t *testing.T) {
	upstream := &fakeEditableUpstream{files: map[string]string{"deck.pptx": "PPTX", "assets.zip": "ZIP"}}
	server, deps := newEditableRouteServer(t, []map[string]any{editableAccount()}, upstream)

	// 在 files 根之外放一个「秘密」文件，任何路径都不该能读到它。
	secret := filepath.Join(deps.Config.DataDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// 先产出一个合法文件，确认 /files/* 本身是通的。
	if status, payload := postJSON(t, server, "/v1/ppt/generations", map[string]any{
		"prompt": "p", "client_task_id": "files-1",
	}); status != http.StatusOK {
		t.Fatalf("submit status = %d, body = %s", status, payload)
	}
	waitEditableHTTPTask(t, server, "files-1")

	response, err := http.Get(server.URL + "/files/ppt/files-1/deck.pptx")
	if err != nil {
		t.Fatalf("GET valid file: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("valid file status = %d, want 200", response.StatusCode)
	}

	for _, path := range []string{
		"/files/../../secret.txt",
		"/files/ppt/../../../secret.txt",
		"/files/%2e%2e/%2e%2e/secret.txt",
		"/files/ppt/files-1/missing.pptx",
	} {
		request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		// 关掉重定向跟随：Go 的 client 会把 ../ 规整掉再发，测不到服务端行为。
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, response.StatusCode)
		}
		if strings.Contains(string(body), "SECRET") {
			t.Errorf("%s leaked the secret file", path)
		}
	}
}

// /v1/editable-file-tasks：ids 过滤 + missing_ids + owner 隔离。
func TestEditableTaskListRoute(t *testing.T) {
	upstream := &fakeEditableUpstream{files: map[string]string{"deck.pptx": "PPTX", "assets.zip": "ZIP"}}
	server, _ := newEditableRouteServer(t, []map[string]any{editableAccount()}, upstream)

	for _, id := range []string{"t1", "t2"} {
		if status, payload := postJSON(t, server, "/v1/ppt/generations", map[string]any{
			"prompt": "p", "client_task_id": id,
		}); status != http.StatusOK {
			t.Fatalf("submit %s status = %d, body = %s", id, status, payload)
		}
		waitEditableHTTPTask(t, server, id)
	}

	status, payload := getWithAuth(t, server, "/v1/editable-file-tasks?ids=t2,t1,missing")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	var body struct {
		Items      []map[string]any `json:"items"`
		MissingIDs []string         `json:"missing_ids"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, payload)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	if stringValueOfTest(body.Items[0]["id"]) != "t2" || stringValueOfTest(body.Items[1]["id"]) != "t1" {
		t.Errorf("items order = %v/%v, want t2/t1", body.Items[0]["id"], body.Items[1]["id"])
	}
	if len(body.MissingIDs) != 1 || body.MissingIDs[0] != "missing" {
		t.Errorf("missing_ids = %v, want [missing]", body.MissingIDs)
	}

	// 不带 ids：返回全部（按 updated_at 倒序）。
	status, payload = getWithAuth(t, server, "/v1/editable-file-tasks")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, payload)
	}
	if len(body.Items) != 2 {
		t.Errorf("items = %d, want 2", len(body.Items))
	}
}

// 两个提交端点都必须写调用日志（日志页按 type=call 展示）。
func TestEditableRouteWritesCallLog(t *testing.T) {
	upstream := &fakeEditableUpstream{files: map[string]string{"deck.pptx": "PPTX", "assets.zip": "ZIP"}}
	server, deps := newEditableRouteServer(t, []map[string]any{editableAccount()}, upstream)

	if status, payload := postJSON(t, server, "/v1/ppt/generations", map[string]any{
		"prompt": "p", "client_task_id": "logged",
	}); status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, payload)
	}
	waitEditableHTTPTask(t, server, "logged")

	logs := deps.Logs.List(service.LogTypeCall, "", "", 50)
	found := false
	for _, item := range logs {
		if strings.Contains(stringValueOfTest(item["summary"]), "PPT生成任务") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no PPT生成任务 call log written; logs = %+v", logs)
	}
}
