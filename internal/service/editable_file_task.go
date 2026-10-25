package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/google/uuid"
)

// 可编辑文件（PPT/PSD）任务队列，复刻 services/editable_file_task_service.py。
//
// 结构与 ImageTaskService 一致（queued/running/success/error + 落盘 + 恢复 +
// Close 等在途），差异只在任务内容：
//   - 产物是文件（pptx/psd + zip），落 data/files/{kind}/{task_id}/
//   - 账号只取 Plus/Team/Pro/Enterprise，成功后 MarkTextUsed
//   - 对外暴露 /files/* 的路径解析（public_file_path），必须防目录穿越

// EditableFilePlanTypes 复刻 EDITABLE_FILE_PLAN_TYPES。
var EditableFilePlanTypes = []string{"Plus", "Team", "Pro", "Enterprise"}

// ErrEditableTaskClosed 表示服务已进入关闭流程，不再接受新任务。
var ErrEditableTaskClosed = errors.New("editable file task service is shutting down")

// EditableAccounts 是可编辑文件任务对号池的最小依赖。
//
// 抽成接口是为了让 service 不直接绑 AccountService（测试可以给假实现），
// 同时保证「只取 Plus/Team/Pro/Enterprise」「成功后 MarkTextUsed」这两条
// 硬要求在 service 层被执行，而不是散落在 api 层。
type EditableAccounts interface {
	// EditableFileAccessToken 选一个可用于导出的 token。
	EditableFileAccessToken() (string, error)
	// EditableFileAccountEmail 取 token 对应账号的邮箱（日志用）。
	EditableFileAccountEmail(token string) string
	// MarkTextUsed 记录账号最近使用时间。
	MarkTextUsed(token string)
}

// EditableFileTaskRunner 是任务执行体，由 api 层注入（service 不依赖 upstream）。
//
// 返回 (结果, 错误)；结果里的三个字段直接进 public task 的 result。
type EditableFileTaskRunner func(ctx EditableFileTaskContext) (EditableFileTaskResult, error)

// EditableFileTaskContext 是一次任务执行需要的全部输入。
type EditableFileTaskContext struct {
	TaskID       string
	Kind         string // protocol.EditableKindPPT / protocol.EditableKindPSD
	Prompt       string
	Base64Images []string
	// OutputDir 是产物落盘目录（data/files/{kind}/{task_id}）。
	OutputDir string
	// BaseURL 是拼 /files/ 链接用的前缀（配置的 base_url，可空）。
	BaseURL string
	// KeyName 是调用方密钥名，只用于调用日志。
	KeyName string
	// AccessToken 是本任务选中的账号 token（由服务在开跑前选号填入）。
	AccessToken string
}

// EditableFileTaskResult 是执行结果。
//
// PrimaryPath / ZipPath 是**本地绝对路径**：链接由服务用 FileURL 拼
// （对应 Python 的 _file_url(result.primary_path, base_url)）。
type EditableFileTaskResult struct {
	ConversationID string
	PrimaryPath    string
	ZipPath        string
	AccountEmail   string
}

// EditableFileTaskService 是可编辑文件任务队列。
type EditableFileTaskService struct {
	// root 是 data/files（与 Python 的 EDITABLE_FILE_ROOT 一致）。
	root string
	path string
	// runner 由 api 层注入（service 不依赖 upstream）。
	runner EditableFileTaskRunner
	// accounts 是号池（选号 + 记账）；nil 时任务直接失败。
	accounts EditableAccounts

	mu    sync.RWMutex
	tasks map[string]map[string]any

	inFlight    sync.WaitGroup
	lifecycleMu sync.Mutex
	closing     bool
	closeDone   chan struct{}
	closeOnce   sync.Once
}

// NewEditableFileTaskService 构造。root 是 data 目录（服务内部会拼 files/）。
func NewEditableFileTaskService(dataDir string) *EditableFileTaskService {
	root := filepath.Join(dataDir, "files")
	_ = os.MkdirAll(root, 0o755)
	s := &EditableFileTaskService{
		root:      root,
		path:      filepath.Join(dataDir, "editable_file_tasks.json"),
		tasks:     map[string]map[string]any{},
		closeDone: make(chan struct{}),
	}
	s.mu.Lock()
	s.tasks = s.loadLocked()
	if s.recoverUnfinishedLocked() {
		s.saveLocked()
	}
	s.mu.Unlock()
	return s
}

// SetRunner 注入执行体。
func (s *EditableFileTaskService) SetRunner(runner EditableFileTaskRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runner = runner
}

// SetAccounts 注入号池。
func (s *EditableFileTaskService) SetAccounts(accounts EditableAccounts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts = accounts
}

// HasRunner 报告执行体是否已注入（装配漏了要能被断言）。
func (s *EditableFileTaskService) HasRunner() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runner != nil
}

// Close 停止接受新任务并等待在途任务退出（语义同 ImageTaskService.Close）。
func (s *EditableFileTaskService) Close(ctx context.Context) error {
	s.lifecycleMu.Lock()
	s.closing = true
	s.lifecycleMu.Unlock()

	s.closeOnce.Do(func() {
		go func() {
			s.inFlight.Wait()
			close(s.closeDone)
		}()
	})

	select {
	case <-s.closeDone:
	case <-ctx.Done():
		return ctx.Err()
	}

	s.mu.Lock()
	s.saveLocked()
	s.mu.Unlock()
	return nil
}

func (s *EditableFileTaskService) track() (release func(), ok bool) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closing {
		return nil, false
	}
	s.inFlight.Add(1)
	return s.inFlight.Done, true
}

// Submit 创建任务并异步执行（复刻 _submit，:115-127）。
//
// 幂等：同一 owner + client_task_id 再次提交直接返回已有任务。
func (s *EditableFileTaskService) Submit(
	ownerID, clientTaskID, keyName, kind, prompt string,
	base64Images []string,
	baseURL string,
) (map[string]any, error) {
	taskID := strings.TrimSpace(clientTaskID)
	if taskID == "" {
		taskID = uuid.NewString()
	}
	owner := orDefault(ownerID, "anonymous")
	key := taskKey(owner, taskID)

	// 先登记再建任务：关闭中的服务连 queued 都不该落盘。
	release, ok := s.track()
	if !ok {
		return nil, ErrEditableTaskClosed
	}

	s.mu.Lock()
	if existing := s.tasks[key]; existing != nil {
		// 必须在锁内构造快照：publicEditableTask 会读 map 的多个字段，
		// 而 finishTask 在同一把锁下改写它。放到 Unlock 之后读就是
		// 数据竞争（-race 可复现）。
		snapshot := publicEditableTask(existing)
		s.mu.Unlock()
		release()
		return snapshot, nil
	}
	now := nowLocal()
	task := map[string]any{
		"id":         taskID,
		"owner_id":   owner,
		"status":     TaskStatusQueued,
		"kind":       kind,
		"model":      protocol.EditableFileModel,
		"created_at": now,
		"updated_at": now,
		"created_ts": float64(time.Now().UnixNano()) / 1e9,
	}
	s.tasks[key] = task
	s.saveLocked()
	snapshot := publicEditableTask(task)
	s.mu.Unlock()

	outputDir := filepath.Join(s.root, kind, taskID)
	go s.runTask(key, EditableFileTaskContext{
		TaskID:       taskID,
		Kind:         kind,
		Prompt:       prompt,
		Base64Images: base64Images,
		OutputDir:    outputDir,
		BaseURL:      baseURL,
		KeyName:      keyName,
	}, release)

	return snapshot, nil
}

// runTask 是任务 goroutine 的主体。release 必须 defer，否则 Close 永远等不到。
func (s *EditableFileTaskService) runTask(key string, taskCtx EditableFileTaskContext, release func()) {
	defer release()

	s.mu.Lock()
	task := s.tasks[key]
	if task == nil {
		s.mu.Unlock()
		return
	}
	task["status"] = TaskStatusRunning
	task["error"] = ""
	task["started_ts"] = float64(time.Now().UnixNano()) / 1e9
	task["updated_at"] = nowLocal()
	task["updated_ts"] = float64(time.Now().UnixNano()) / 1e9
	runner := s.runner
	accounts := s.accounts
	s.saveLocked()
	s.mu.Unlock()

	if runner == nil {
		s.finishTask(key, EditableFileTaskResult{}, errors.New("editable file task runner is not configured"))
		return
	}
	if accounts == nil {
		s.finishTask(key, EditableFileTaskResult{}, errors.New("editable file account pool is not configured"))
		return
	}

	// PSD 必须有参考图（Python 在 export_psd_zip 里抛同一条文案）。
	// 放在选号之前：参数错误不该消耗账号。
	if taskCtx.Kind == protocol.EditableKindPSD && len(taskCtx.Base64Images) == 0 {
		s.finishTask(key, EditableFileTaskResult{}, errors.New("base64_images is empty"))
		return
	}

	token, err := accounts.EditableFileAccessToken()
	if err != nil {
		s.finishTask(key, EditableFileTaskResult{}, err)
		return
	}
	taskCtx.AccessToken = token
	accountEmail := accounts.EditableFileAccountEmail(token)

	result, runErr := runner(taskCtx)
	if runErr != nil {
		result.AccountEmail = accountEmail
		s.finishTask(key, result, runErr)
		return
	}
	// 产物链接在这里拼：与 Python 一样，任务服务负责把本地路径转成对外 URL。
	result.PrimaryPath = s.FileURL(result.PrimaryPath, taskCtx.BaseURL)
	result.ZipPath = s.FileURL(result.ZipPath, taskCtx.BaseURL)
	if result.PrimaryPath == "" || result.ZipPath == "" {
		s.finishTask(key, result, fmt.Errorf("exported files are outside the editable file root"))
		return
	}
	// 只有成功才记账（与 Python 的 mark_text_used 位置一致）。
	accounts.MarkTextUsed(token)
	result.AccountEmail = accountEmail
	s.finishTask(key, result, nil)
}

func (s *EditableFileTaskService) finishTask(key string, result EditableFileTaskResult, taskErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[key]
	if task == nil {
		return
	}
	task["updated_at"] = nowLocal()
	task["updated_ts"] = float64(time.Now().UnixNano()) / 1e9
	task["ended_ts"] = float64(time.Now().UnixNano()) / 1e9
	if taskErr != nil {
		message := strings.TrimSpace(taskErr.Error())
		if message == "" {
			message = "editable file task failed"
		}
		task["status"] = TaskStatusError
		task["error"] = message
		if email := strings.TrimSpace(result.AccountEmail); email != "" {
			task["account_email"] = email
		}
		s.saveLocked()
		return
	}
	task["status"] = TaskStatusSuccess
	task["error"] = ""
	task["result"] = map[string]any{
		"conversation_id": result.ConversationID,
		"primary_url":     result.PrimaryPath,
		"zip_url":         result.ZipPath,
	}
	if email := strings.TrimSpace(result.AccountEmail); email != "" {
		task["account_email"] = email
	}
	s.saveLocked()
}

// FileURL 复刻 _file_url（:50-53）：把产物绝对路径转成 /files/... 链接。
//
// 入参是「绝对路径」；返回空串表示路径不在 root 下（不该发生，但不静默）。
// 执行体（api 层）用它拼 result，这样调用日志里也能带 URL（与 Python 一致）。
func (s *EditableFileTaskService) FileURL(absolutePath, baseURL string) string {
	if strings.TrimSpace(absolutePath) == "" {
		return ""
	}
	rel, err := filepath.Rel(s.root, absolutePath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	rel = filepath.ToSlash(rel)
	prefix := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	encoded := escapeFileRelPath(rel)
	if prefix == "" {
		return "/files/" + encoded
	}
	return prefix + "/files/" + encoded
}

// escapeFileRelPath 复刻 Python 的 quote(rel, safe='/')：逐段转义，保留斜杠。
func escapeFileRelPath(rel string) string {
	segments := strings.Split(rel, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// ListTasks 复刻 list_tasks（:101-113）：按 owner 过滤；指定 ids 时按请求
// 顺序返回 + missing_ids；未指定时按 updated_at 倒序。
func (s *EditableFileTaskService) ListTasks(ownerID string, taskIDs []string) map[string]any {
	owner := orDefault(ownerID, "anonymous")
	s.mu.RLock()
	defer s.mu.RUnlock()

	requested := make([]string, 0, len(taskIDs))
	for _, item := range taskIDs {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			requested = append(requested, trimmed)
		}
	}

	if len(requested) > 0 {
		items := make([]map[string]any, 0, len(requested))
		missing := make([]string, 0)
		for _, taskID := range requested {
			task := s.tasks[taskKey(owner, taskID)]
			if task == nil {
				missing = append(missing, taskID)
				continue
			}
			items = append(items, publicEditableTask(task))
		}
		return map[string]any{"items": items, "missing_ids": missing}
	}

	items := make([]map[string]any, 0)
	for _, task := range s.tasks {
		if stringValue(task["owner_id"]) != owner {
			continue
		}
		items = append(items, publicEditableTask(task))
	}
	sort.SliceStable(items, func(i, j int) bool {
		return stringValue(items[i]["updated_at"]) > stringValue(items[j]["updated_at"])
	})
	return map[string]any{"items": items, "missing_ids": []string{}}
}

// GetTask 返回内部任务记录副本（测试与诊断用）。
func (s *EditableFileTaskService) GetTask(ownerID, taskID string) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task := s.tasks[taskKey(orDefault(ownerID, "anonymous"), strings.TrimSpace(taskID))]
	if task == nil {
		return nil
	}
	return copyMap(task)
}

// PublicFilePath 复刻 public_file_path（:159-165）：把相对路径解析成
// data/files 下的真实文件，越界与不存在都返回错误。
//
// 防穿越是硬要求：Python 用 Path.resolve() + relative_to() 断言，
// 这里用 filepath.Rel + 前缀校验，并额外解析符号链接
// （data/files/ 下指向 config.json 的软链同样必须被拒）。
func (s *EditableFileTaskService) PublicFilePath(relativePath string) (string, error) {
	raw := strings.TrimLeft(strings.ReplaceAll(relativePath, "\\", "/"), "/")
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("file not found")
	}
	root, err := filepath.Abs(s.root)
	if err != nil {
		return "", err
	}
	full, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(raw)))
	if err != nil {
		return "", err
	}
	if !pathWithin(root, full) {
		return "", fmt.Errorf("file not found")
	}

	resolvedRoot, rootErr := filepath.EvalSymlinks(root)
	resolvedFull, fullErr := filepath.EvalSymlinks(full)
	if rootErr == nil && fullErr == nil && !pathWithin(resolvedRoot, resolvedFull) {
		return "", fmt.Errorf("file not found")
	}

	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("file not found")
	}
	return full, nil
}

// pathWithin 报告 target 是否等于 root 或位于 root 之下。
func pathWithin(root, target string) bool {
	if target == root {
		return true
	}
	return strings.HasPrefix(target, root+string(filepath.Separator))
}

func (s *EditableFileTaskService) loadLocked() map[string]map[string]any {
	payload, err := os.ReadFile(s.path)
	if err != nil {
		return map[string]map[string]any{}
	}
	var wrapper struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		return map[string]map[string]any{}
	}
	out := make(map[string]map[string]any, len(wrapper.Tasks))
	for _, item := range wrapper.Tasks {
		taskID := strings.TrimSpace(stringValue(item["id"]))
		owner := strings.TrimSpace(stringValue(item["owner_id"]))
		if taskID == "" || owner == "" {
			continue
		}
		status := strings.TrimSpace(stringValue(item["status"]))
		if !taskUnfinished[status] && !taskTerminal[status] {
			status = TaskStatusError
		}
		kind := protocol.EditableKindPPT
		if stringValue(item["kind"]) == protocol.EditableKindPSD {
			kind = protocol.EditableKindPSD
		}
		createdAt := strings.TrimSpace(stringValue(item["created_at"]))
		if createdAt == "" {
			createdAt = nowLocal()
		}
		updatedAt := strings.TrimSpace(stringValue(item["updated_at"]))
		if updatedAt == "" {
			updatedAt = createdAt
		}
		task := map[string]any{
			"id":         taskID,
			"owner_id":   owner,
			"status":     status,
			"kind":       kind,
			"model":      orDefault(stringValue(item["model"]), protocol.EditableFileModel),
			"created_at": createdAt,
			"updated_at": updatedAt,
			"created_ts": item["created_ts"],
			"updated_ts": item["updated_ts"],
		}
		if result, ok := item["result"].(map[string]any); ok && len(result) > 0 {
			task["result"] = result
		}
		if errText := strings.TrimSpace(stringValue(item["error"])); errText != "" {
			task["error"] = errText
		}
		if email := strings.TrimSpace(stringValue(item["account_email"])); email != "" {
			task["account_email"] = email
		}
		for _, field := range []string{"started_ts", "ended_ts"} {
			if value, ok := item[field]; ok && value != nil {
				task[field] = value
			}
		}
		out[taskKey(owner, taskID)] = task
	}
	return out
}

func (s *EditableFileTaskService) saveLocked() {
	items := make([]map[string]any, 0, len(s.tasks))
	for _, task := range s.tasks {
		items = append(items, task)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return stringValue(items[i]["updated_at"]) > stringValue(items[j]["updated_at"])
	})
	payload, err := json.MarshalIndent(map[string]any{"tasks": items}, "", "  ")
	if err != nil {
		return
	}
	payload = append(payload, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		_ = os.WriteFile(s.path, payload, 0o644)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.WriteFile(s.path, payload, 0o644)
		_ = os.Remove(tmp)
	}
}

// recoverUnfinishedLocked 复刻「服务重启，未完成任务置 error」。
func (s *EditableFileTaskService) recoverUnfinishedLocked() bool {
	changed := false
	for _, task := range s.tasks {
		if !taskUnfinished[stringValue(task["status"])] {
			continue
		}
		task["status"] = TaskStatusError
		task["error"] = "服务已重启，未完成的任务已中断"
		task["updated_at"] = nowLocal()
		task["updated_ts"] = float64(time.Now().UnixNano()) / 1e9
		changed = true
	}
	return changed
}

// publicEditableTask 复刻 _public_task（:68-82）：
// id/taskId/status/kind/created_at/updated_at/elapsed_seconds + result/error。
func publicEditableTask(task map[string]any) map[string]any {
	item := map[string]any{
		"id":              task["id"],
		"taskId":          task["id"],
		"status":          task["status"],
		"kind":            task["kind"],
		"created_at":      task["created_at"],
		"updated_at":      task["updated_at"],
		"elapsed_seconds": editableElapsedSeconds(task),
	}
	if result, ok := task["result"]; ok && result != nil {
		item["result"] = result
	}
	if errText := stringValue(task["error"]); errText != "" {
		item["error"] = errText
	}
	return item
}

// editableElapsedSeconds 复刻 _elapsed_seconds（:42-47）。
func editableElapsedSeconds(task map[string]any) int {
	start := floatValue(task["started_ts"])
	if start == 0 {
		start = floatValue(task["created_ts"])
	}
	if start == 0 {
		return 0
	}
	end := floatValue(task["ended_ts"])
	if end == 0 {
		end = float64(time.Now().UnixNano()) / 1e9
	}
	elapsed := int(end - start)
	if elapsed < 0 {
		return 0
	}
	return elapsed
}
