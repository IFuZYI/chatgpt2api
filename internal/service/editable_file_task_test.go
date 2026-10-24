package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// editableFakeAccounts 是 EditableAccounts 的测试替身。
type editableFakeAccounts struct {
	mu sync.Mutex

	token   string
	err     error
	email   string
	marked  []string
	markAll bool
}

func (f *editableFakeAccounts) EditableFileAccessToken() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
}

func (f *editableFakeAccounts) EditableFileAccountEmail(string) string { return f.email }

func (f *editableFakeAccounts) MarkTextUsed(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked = append(f.marked, token)
}

func (f *editableFakeAccounts) markedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.marked...)
}

// newEditableTestService 建一个可编辑文件任务服务（带关闭清理）。
func newEditableTestService(t *testing.T) *EditableFileTaskService {
	t.Helper()
	svc, _ := newEditableTestServiceWithDir(t)
	return svc
}

// newEditableTestServiceWithDir 同时返回 data 目录（测试要往里放文件）。
func newEditableTestServiceWithDir(t *testing.T) (*EditableFileTaskService, string) {
	t.Helper()
	dir := t.TempDir()
	svc := NewEditableFileTaskService(dir)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := svc.Close(ctx); err != nil {
			t.Errorf("close editable tasks: %v", err)
		}
	})
	return svc, dir
}

// waitEditableTask 轮询任务直到终态（测试里避免依赖固定 sleep）。
func waitEditableTask(t *testing.T, svc *EditableFileTaskService, owner, taskID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task := svc.GetTask(owner, taskID)
		if task != nil && IsTaskTerminal(stringValue(task["status"])) {
			return task
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach a terminal state", taskID)
	return nil
}

// 成功路径：runner 拿到选号 token、产物链接被拼成 /files/...、MarkTextUsed 被调用。
func TestEditableTaskSubmitRunsRunnerAndMarksAccountUsed(t *testing.T) {
	svc := newEditableTestService(t)
	accounts := &editableFakeAccounts{token: "tok-plus", email: "plus@example.com"}
	svc.SetAccounts(accounts)

	var got EditableFileTaskContext
	svc.SetRunner(func(taskCtx EditableFileTaskContext) (EditableFileTaskResult, error) {
		got = taskCtx
		return EditableFileTaskResult{
			ConversationID: "cid-1",
			PrimaryPath:    filepath.Join(taskCtx.OutputDir, "deck.pptx"),
			ZipPath:        filepath.Join(taskCtx.OutputDir, "assets.zip"),
		}, nil
	})

	snapshot, err := svc.Submit("owner", "task-1", "密钥A", "ppt", "做PPT", nil, "https://api.example")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if stringValue(snapshot["status"]) != TaskStatusQueued {
		t.Errorf("initial status = %q, want queued", stringValue(snapshot["status"]))
	}

	task := waitEditableTask(t, svc, "owner", "task-1")
	if stringValue(task["status"]) != TaskStatusSuccess {
		t.Fatalf("status = %q, error = %q, want success", stringValue(task["status"]), stringValue(task["error"]))
	}
	if got.AccessToken != "tok-plus" {
		t.Errorf("runner token = %q, want tok-plus", got.AccessToken)
	}
	if got.Kind != "ppt" || got.Prompt != "做PPT" {
		t.Errorf("runner context = %+v, want kind=ppt prompt=做PPT", got)
	}
	if got.KeyName != "密钥A" {
		t.Errorf("runner key name = %q, want 密钥A", got.KeyName)
	}
	if want := filepath.Join(svc.root, "ppt", "task-1"); got.OutputDir != want {
		t.Errorf("output dir = %q, want %q", got.OutputDir, want)
	}

	result, _ := task["result"].(map[string]any)
	if stringValue(result["conversation_id"]) != "cid-1" {
		t.Errorf("result conversation_id = %v, want cid-1", result["conversation_id"])
	}
	if stringValue(result["primary_url"]) != "https://api.example/files/ppt/task-1/deck.pptx" {
		t.Errorf("primary_url = %q", stringValue(result["primary_url"]))
	}
	if stringValue(result["zip_url"]) != "https://api.example/files/ppt/task-1/assets.zip" {
		t.Errorf("zip_url = %q", stringValue(result["zip_url"]))
	}
	if marked := accounts.markedTokens(); len(marked) != 1 || marked[0] != "tok-plus" {
		t.Errorf("MarkTextUsed calls = %v, want [tok-plus]", marked)
	}

	public := publicEditableTask(task)
	for _, key := range []string{"id", "taskId", "status", "kind", "created_at", "updated_at", "elapsed_seconds"} {
		if _, ok := public[key]; !ok {
			t.Errorf("public task missing %q", key)
		}
	}
	if stringValue(public["kind"]) != "ppt" {
		t.Errorf("public kind = %q, want ppt", stringValue(public["kind"]))
	}
}

// 失败路径：runner 报错 → status=error、错误文案进任务、不记账。
func TestEditableTaskFailureDoesNotMarkAccountUsed(t *testing.T) {
	svc := newEditableTestService(t)
	accounts := &editableFakeAccounts{token: "tok-plus", email: "plus@example.com"}
	svc.SetAccounts(accounts)
	svc.SetRunner(func(EditableFileTaskContext) (EditableFileTaskResult, error) {
		return EditableFileTaskResult{}, errors.New("upstream exploded")
	})

	if _, err := svc.Submit("owner", "task-err", "", "ppt", "p", nil, ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	task := waitEditableTask(t, svc, "owner", "task-err")
	if stringValue(task["status"]) != TaskStatusError {
		t.Fatalf("status = %q, want error", stringValue(task["status"]))
	}
	if stringValue(task["error"]) != "upstream exploded" {
		t.Errorf("error = %q, want upstream exploded", stringValue(task["error"]))
	}
	if marked := accounts.markedTokens(); len(marked) != 0 {
		t.Errorf("MarkTextUsed must not run on failure, got %v", marked)
	}
}

// PSD 无参考图：报 base64_images is empty，且不消耗账号（选号在参数校验之后）。
func TestEditableTaskPSDWithoutImagesFails(t *testing.T) {
	svc := newEditableTestService(t)
	accounts := &editableFakeAccounts{token: "tok-plus"}
	svc.SetAccounts(accounts)
	svc.SetRunner(func(EditableFileTaskContext) (EditableFileTaskResult, error) {
		t.Error("runner must not run for a PSD task without images")
		return EditableFileTaskResult{}, nil
	})

	if _, err := svc.Submit("owner", "task-psd", "", "psd", "p", nil, ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	task := waitEditableTask(t, svc, "owner", "task-psd")
	if stringValue(task["status"]) != TaskStatusError || stringValue(task["error"]) != "base64_images is empty" {
		t.Fatalf("task = %+v, want error base64_images is empty", task)
	}
	if marked := accounts.markedTokens(); len(marked) != 0 {
		t.Errorf("MarkTextUsed must not run, got %v", marked)
	}
}

// 号池为空：任务失败并带 Python 的原文案。
func TestEditableTaskWithoutAccountsFails(t *testing.T) {
	svc := newEditableTestService(t)
	svc.SetAccounts(&editableFakeAccounts{err: ErrNoEditableAccount})
	svc.SetRunner(func(EditableFileTaskContext) (EditableFileTaskResult, error) {
		t.Error("runner must not run without an account")
		return EditableFileTaskResult{}, nil
	})

	if _, err := svc.Submit("owner", "task-noacct", "", "ppt", "p", nil, ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	task := waitEditableTask(t, svc, "owner", "task-noacct")
	if stringValue(task["error"]) != "no available plus/team/pro account" {
		t.Fatalf("error = %q, want no available plus/team/pro account", stringValue(task["error"]))
	}
}

// 幂等：同一 client_task_id 提交两次返回同一任务，runner 只跑一次。
func TestEditableTaskSubmitIsIdempotent(t *testing.T) {
	svc := newEditableTestService(t)
	svc.SetAccounts(&editableFakeAccounts{token: "tok-plus"})

	var runs int
	var runsMu sync.Mutex
	svc.SetRunner(func(EditableFileTaskContext) (EditableFileTaskResult, error) {
		runsMu.Lock()
		runs++
		runsMu.Unlock()
		return EditableFileTaskResult{ConversationID: "cid-1"}, nil
	})

	first, err := svc.Submit("owner", "same-id", "", "ppt", "p", nil, "")
	if err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	second, err := svc.Submit("owner", "same-id", "", "ppt", "p", nil, "")
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if stringValue(first["id"]) != stringValue(second["id"]) {
		t.Fatalf("ids differ: %v vs %v", first["id"], second["id"])
	}
	waitEditableTask(t, svc, "owner", "same-id")

	runsMu.Lock()
	defer runsMu.Unlock()
	if runs != 1 {
		t.Errorf("runner runs = %d, want 1", runs)
	}
}

// 同一 task_id 在不同 owner 下是不同任务。
func TestEditableTaskIdsAreScopedByOwner(t *testing.T) {
	svc := newEditableTestService(t)
	svc.SetAccounts(&editableFakeAccounts{token: "tok-plus"})
	svc.SetRunner(func(EditableFileTaskContext) (EditableFileTaskResult, error) {
		return EditableFileTaskResult{ConversationID: "cid-1"}, nil
	})

	if _, err := svc.Submit("owner-a", "shared-id", "", "ppt", "p", nil, ""); err != nil {
		t.Fatalf("Submit owner-a: %v", err)
	}
	if _, err := svc.Submit("owner-b", "shared-id", "", "ppt", "p", nil, ""); err != nil {
		t.Fatalf("Submit owner-b: %v", err)
	}
	waitEditableTask(t, svc, "owner-a", "shared-id")
	waitEditableTask(t, svc, "owner-b", "shared-id")

	items := svc.ListTasks("owner-a", nil)["items"].([]map[string]any)
	if len(items) != 1 {
		t.Fatalf("owner-a items = %d, want 1", len(items))
	}
}

// Close 必须等在途任务真正结束。
func TestEditableTaskCloseWaitsForRunningTask(t *testing.T) {
	svc := NewEditableFileTaskService(t.TempDir())
	svc.SetAccounts(&editableFakeAccounts{token: "tok-plus"})

	finished := make(chan struct{})
	svc.SetRunner(func(EditableFileTaskContext) (EditableFileTaskResult, error) {
		time.Sleep(120 * time.Millisecond)
		close(finished)
		return EditableFileTaskResult{ConversationID: "cid-1"}, nil
	})

	if _, err := svc.Submit("owner", "slow", "", "ppt", "p", nil, ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("Close returned while the task goroutine was still running")
	}
}

// 关闭后 Submit 必须明确拒绝。
func TestEditableTaskCloseRejectsNewSubmit(t *testing.T) {
	svc := NewEditableFileTaskService(t.TempDir())
	svc.SetAccounts(&editableFakeAccounts{token: "tok-plus"})
	svc.SetRunner(func(EditableFileTaskContext) (EditableFileTaskResult, error) {
		return EditableFileTaskResult{}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := svc.Submit("owner", "after-close", "", "ppt", "p", nil, ""); !errors.Is(err, ErrEditableTaskClosed) {
		t.Fatalf("Submit after Close error = %v, want ErrEditableTaskClosed", err)
	}
}

// 重启恢复：未完成任务置 error。
func TestEditableTaskRecoversUnfinishedOnRestart(t *testing.T) {
	dir := t.TempDir()
	svc := NewEditableFileTaskService(dir)
	svc.SetAccounts(&editableFakeAccounts{token: "tok-plus"})

	blocked := make(chan struct{})
	svc.SetRunner(func(EditableFileTaskContext) (EditableFileTaskResult, error) {
		<-blocked
		return EditableFileTaskResult{}, nil
	})
	if _, err := svc.Submit("owner", "stuck", "", "ppt", "p", nil, ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// 等任务进入 running，确保磁盘上是 running 而不是 queued。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if task := svc.GetTask("owner", "stuck"); task != nil && stringValue(task["status"]) == TaskStatusRunning {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// 新服务读同一份任务文件（模拟进程重启）。
	restarted := NewEditableFileTaskService(dir)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = restarted.Close(ctx)
	})

	task := restarted.GetTask("owner", "stuck")
	if task == nil {
		t.Fatal("restarted service lost the task")
	}
	if stringValue(task["status"]) != TaskStatusError {
		t.Errorf("recovered status = %q, want error", stringValue(task["status"]))
	}
	if stringValue(task["error"]) != "服务已重启，未完成的任务已中断" {
		t.Errorf("recovered error = %q", stringValue(task["error"]))
	}

	close(blocked)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = svc.Close(ctx)
}

// 任务落盘：重启后仍能读到 success 任务与其 result。
func TestEditableTaskPersistsSuccess(t *testing.T) {
	dir := t.TempDir()
	svc := NewEditableFileTaskService(dir)
	svc.SetAccounts(&editableFakeAccounts{token: "tok-plus", email: "a@b.c"})
	svc.SetRunner(func(taskCtx EditableFileTaskContext) (EditableFileTaskResult, error) {
		return EditableFileTaskResult{
			ConversationID: "cid-9",
			PrimaryPath:    filepath.Join(taskCtx.OutputDir, "deck.pptx"),
			ZipPath:        filepath.Join(taskCtx.OutputDir, "assets.zip"),
		}, nil
	})
	if _, err := svc.Submit("owner", "persist", "", "ppt", "p", nil, ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitEditableTask(t, svc, "owner", "persist")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	restarted := NewEditableFileTaskService(dir)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = restarted.Close(closeCtx)
	})
	task := restarted.GetTask("owner", "persist")
	if task == nil {
		t.Fatal("restarted service lost the task")
	}
	if stringValue(task["status"]) != TaskStatusSuccess {
		t.Fatalf("status = %q, want success", stringValue(task["status"]))
	}
	result, _ := task["result"].(map[string]any)
	if stringValue(result["conversation_id"]) != "cid-9" {
		t.Errorf("result = %+v, want conversation_id cid-9", result)
	}
}

// ListTasks：指定 ids 时按请求顺序返回 + missing_ids；未指定时按 updated_at 倒序。
func TestEditableTaskListTasks(t *testing.T) {
	svc := newEditableTestService(t)
	svc.SetAccounts(&editableFakeAccounts{token: "tok-plus"})
	svc.SetRunner(func(EditableFileTaskContext) (EditableFileTaskResult, error) {
		return EditableFileTaskResult{ConversationID: "cid-1"}, nil
	})

	for _, id := range []string{"a", "b"} {
		if _, err := svc.Submit("owner", id, "", "ppt", "p", nil, ""); err != nil {
			t.Fatalf("Submit %s: %v", id, err)
		}
		waitEditableTask(t, svc, "owner", id)
	}
	// 别的 owner 的任务不能出现在结果里。
	if _, err := svc.Submit("someone-else", "c", "", "ppt", "p", nil, ""); err != nil {
		t.Fatalf("Submit c: %v", err)
	}
	waitEditableTask(t, svc, "someone-else", "c")

	result := svc.ListTasks("owner", []string{"b", "a", "missing"})
	items, _ := result["items"].([]map[string]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if stringValue(items[0]["id"]) != "b" || stringValue(items[1]["id"]) != "a" {
		t.Errorf("items order = %v/%v, want b/a (request order)",
			stringValue(items[0]["id"]), stringValue(items[1]["id"]))
	}
	missing, _ := result["missing_ids"].([]string)
	if len(missing) != 1 || missing[0] != "missing" {
		t.Errorf("missing_ids = %v, want [missing]", missing)
	}

	all := svc.ListTasks("owner", nil)
	allItems, _ := all["items"].([]map[string]any)
	if len(allItems) != 2 {
		t.Errorf("all items = %d, want 2 (owner isolation)", len(allItems))
	}
}

// public_file_path 必须防目录穿越（Python 用 relative_to 断言）。
func TestEditablePublicFilePathRejectsTraversal(t *testing.T) {
	svc, _ := newEditableTestServiceWithDir(t)
	if err := os.MkdirAll(filepath.Join(svc.root, "ppt", "t1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	valid := filepath.Join(svc.root, "ppt", "t1", "deck.pptx")
	if err := os.WriteFile(valid, []byte("DECK"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := svc.PublicFilePath("ppt/t1/deck.pptx")
	if err != nil {
		t.Fatalf("PublicFilePath(valid): %v", err)
	}
	if got != valid {
		t.Errorf("resolved = %q, want %q", got, valid)
	}

	// 前导斜杠与反斜杠都要归一化（Python 的 lstrip("/") + replace("\\", "/")）。
	if _, err := svc.PublicFilePath("/ppt/t1/deck.pptx"); err != nil {
		t.Errorf("leading slash should be tolerated: %v", err)
	}
	if _, err := svc.PublicFilePath(`ppt\t1\deck.pptx`); err != nil {
		t.Errorf("backslashes should be normalized: %v", err)
	}

	for _, candidate := range []string{
		"../../etc/passwd",
		"ppt/../../../etc/passwd",
		"..",
		"",
		"ppt/t1",
		"ppt/t1/missing.pptx",
	} {
		if _, err := svc.PublicFilePath(candidate); err == nil {
			t.Errorf("PublicFilePath(%q) should fail", candidate)
		}
	}
}

// 符号链接越界必须被拒（data/files 下指向外部文件的软链）。
func TestEditablePublicFilePathRejectsSymlinkEscape(t *testing.T) {
	svc, _ := newEditableTestServiceWithDir(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	link := filepath.Join(svc.root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	if _, err := svc.PublicFilePath("link.txt"); err == nil {
		t.Fatal("symlink pointing outside the root must be rejected")
	}
}

// FileURL：把绝对路径转成 /files/... 链接；越界路径返回空串。
func TestEditableFileURL(t *testing.T) {
	svc, _ := newEditableTestServiceWithDir(t)
	inside := filepath.Join(svc.root, "ppt", "t1", "deck.pptx")

	if got := svc.FileURL(inside, ""); got != "/files/ppt/t1/deck.pptx" {
		t.Errorf("FileURL(no base) = %q, want /files/ppt/t1/deck.pptx", got)
	}
	if got := svc.FileURL(inside, "https://api.example/"); got != "https://api.example/files/ppt/t1/deck.pptx" {
		t.Errorf("FileURL(base) = %q", got)
	}
	if got := svc.FileURL("/etc/passwd", ""); got != "" {
		t.Errorf("FileURL(outside) = %q, want empty", got)
	}
	// 文件名里的空格与中文要转义（Python 用 quote(rel, safe='/')）。
	spaced := filepath.Join(svc.root, "ppt", "t1", "我的 演示.pptx")
	if got := svc.FileURL(spaced, ""); got != "/files/ppt/t1/%E6%88%91%E7%9A%84%20%E6%BC%94%E7%A4%BA.pptx" {
		t.Errorf("FileURL(escaped) = %q", got)
	}
}

// 没有 runner 时任务明确失败（装配漏了要能看出来）。
func TestEditableTaskWithoutRunnerFails(t *testing.T) {
	svc := newEditableTestService(t)
	svc.SetAccounts(&editableFakeAccounts{token: "tok-plus"})
	if _, err := svc.Submit("owner", "no-runner", "", "ppt", "p", nil, ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	task := waitEditableTask(t, svc, "owner", "no-runner")
	if stringValue(task["error"]) != "editable file task runner is not configured" {
		t.Fatalf("error = %q, want runner not configured", stringValue(task["error"]))
	}
}
