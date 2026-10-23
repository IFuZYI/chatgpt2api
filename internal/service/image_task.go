package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrImageTaskClosed 表示服务已进入关闭流程，不再接受新任务。
//
// 关闭中继续接受任务会让新 goroutine 在「已经等过一轮」的 WaitGroup 上 Add，
// 于是任务既不被等待、也不会被日志统计到 —— 正是 image_tasks.json 被写坏的路径。
var ErrImageTaskClosed = errors.New("image task service is shutting down")

// 图片任务状态机。这四个字面量是 data/image_tasks.json 与前端契约。
const (
	TaskStatusQueued  = "queued"
	TaskStatusRunning = "running"
	TaskStatusSuccess = "success"
	TaskStatusError   = "error"
)

var taskUnfinished = map[string]bool{TaskStatusQueued: true, TaskStatusRunning: true}
var taskTerminal = map[string]bool{TaskStatusSuccess: true, TaskStatusError: true}

// TaskRunner 是任务执行体，由 protocol 层注入（避免 service 依赖 protocol）。
// 返回 (结果, 错误)；结果里的 data / usage 直接进 public task。
type TaskRunner func(ctx TaskContext) (TaskResult, error)

// TaskContext 是执行一张图所需的一切。
type TaskContext struct {
	TaskID   string
	Mode     string // generate | edit
	Prompt   string
	Model    string
	Size     string
	Quality  string
	BaseURL  string
	Images   [][3]string // (base64, filename, mime)
	Masks    [][3]string
	Progress func(string)
}

// TaskResult 是执行结果。
type TaskResult struct {
	Data           []map[string]any
	Usage          map[string]any
	Error          string
	ConversationID string
}

// ImageTaskService 复刻 services/image_task_service.py 的队列与持久化。
//
// 每个任务一个 goroutine（对应 Python 的 threading.Thread），
// 状态落 data/image_tasks.json，字段集与 _load_locked/_save_locked 一致。
//
// 生命周期：Submit 起来的 goroutine 全部登记在 inFlight 里，Close 等它们退出。
// Python 版靠进程退出时 OS 收走线程；Go 版必须显式等待，否则进程退出时
// image_tasks.json 可能写到一半（临时文件已 rename、内容还没落盘），
// 表现为重启后任务文件损坏或整份任务丢失。
type ImageTaskService struct {
	cfg interface {
		DataDir() string
		ImageRetentionDays() int
	}
	path      string
	generator TaskRunner
	editor    TaskRunner

	mu    sync.RWMutex
	tasks map[string]map[string]any

	// inFlight 登记在跑的任务 goroutine（含 resume-poll 之类的外部任务）。
	inFlight sync.WaitGroup
	// lifecycleMu 让「检查 closing + WaitGroup.Add」成为原子动作。
	//
	// 少了这把锁会有这个窗口：Submit 通过 closing 检查 → Close 置位并 Wait
	// （计数器为 0，立即返回）→ Submit 才 Add 并起 goroutine。于是任务既不被
	// 等待也不被日志统计，正是本方法要消除的那类漏网。
	lifecycleMu sync.Mutex
	closing     bool
	// closeDone 在首次 Close 时由后台 goroutine 关闭，之后所有 Close 调用
	// 都等同一个 channel —— 这样重复调用不会各自 Wait 一份（第二次 Wait
	// 永远等不到已经过去的 Add）。
	closeDone chan struct{}
	closeOnce sync.Once
}

// NewImageTaskService 构造。runner 为 nil 时任务只会停在 queued（M4 前）。
func NewImageTaskService(cfg interface {
	DataDir() string
	ImageRetentionDays() int
}) *ImageTaskService {
	path := cfg.DataDir() + "/image_tasks.json"
	s := &ImageTaskService{
		cfg:       cfg,
		path:      path,
		tasks:     map[string]map[string]any{},
		closeDone: make(chan struct{}),
	}
	s.mu.Lock()
	s.tasks = s.loadLocked()
	if s.recoverUnfinishedLocked() || s.cleanupLocked() {
		s.saveLocked()
	}
	s.mu.Unlock()
	return s
}

// Close 停止接受新任务，并等待所有在途任务 goroutine 退出。
//
// 语义与 http.Server.Shutdown 对齐：ctx 到期即返回 ctx.Err()，但不强杀任务
// （生图请求打到一半被砍，上游那边会留下孤儿对话，反而更难清理）。
// 超时后仍在跑的任务会被记录日志，不会静默吞掉。
//
// 幂等：重复调用只有第一次真正等待，后续调用立即返回 nil。
func (s *ImageTaskService) Close(ctx context.Context) error {
	s.lifecycleMu.Lock()
	s.closing = true
	s.lifecycleMu.Unlock()

	// 只起一个等待者；后续 Close 调用等同一个 channel。
	s.closeOnce.Do(func() {
		go func() {
			s.inFlight.Wait()
			close(s.closeDone)
		}()
	})

	select {
	case <-s.closeDone:
		s.mu.Lock()
		s.saveLocked()
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		pending := s.pendingCount()
		fmt.Printf("[image-task] close timed out with %d task(s) still running; "+
			"their results may be lost (image_tasks.json will be re-checked on next start)\n", pending)
		return ctx.Err()
	}
}

// pendingCount 统计仍处于 queued/running 的任务数（仅用于超时日志）。
func (s *ImageTaskService) pendingCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, task := range s.tasks {
		if taskUnfinished[stringValue(task["status"])] {
			count++
		}
	}
	return count
}

// track 登记一个后台任务；服务已关闭时返回 false。
//
// Add 必须发生在起 goroutine 之前，且与 Close 的置位在同一把锁下，
// 否则会出现「Wait 已返回、Add 才到」的漏网窗口。
func (s *ImageTaskService) track() (release func(), ok bool) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closing {
		return nil, false
	}
	s.inFlight.Add(1)
	return s.inFlight.Done, true
}

// TrackBackground 供 api 层登记自己起的后台 goroutine（如 resume-poll），
// 让它们也纳入关闭等待。服务已关闭时返回 false，调用方应放弃执行。
func (s *ImageTaskService) TrackBackground() (release func(), ok bool) {
	return s.track()
}

// SetRunners 注入生成/编辑执行体。
func (s *ImageTaskService) SetRunners(generate, edit TaskRunner) {
	s.generator = generate
	s.editor = edit
}

func (s *ImageTaskService) loadLocked() map[string]map[string]any {
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
		mode := "generate"
		if stringValue(item["mode"]) == "edit" {
			mode = "edit"
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
			"id":          taskID,
			"owner_id":    owner,
			"status":      status,
			"mode":        mode,
			"model":       orDefault(stringValue(item["model"]), "gpt-image-2"),
			"size":        strings.TrimSpace(stringValue(item["size"])),
			"quality":     orDefault(stringValue(item["quality"]), "auto"),
			"created_at":  createdAt,
			"updated_at":  updatedAt,
			"created_ts":  item["created_ts"],
			"updated_ts":  item["updated_ts"],
			"started_ts":  item["started_ts"],
			"duration_ms": item["duration_ms"],
		}
		if data, ok := item["data"].([]any); ok {
			task["data"] = data
		}
		if usage, ok := item["usage"].(map[string]any); ok {
			task["usage"] = usage
		}
		if errText := strings.TrimSpace(stringValue(item["error"])); errText != "" {
			task["error"] = errText
		}
		out[taskKey(owner, taskID)] = task
	}
	return out
}

func (s *ImageTaskService) saveLocked() {
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
func (s *ImageTaskService) recoverUnfinishedLocked() bool {
	changed := false
	for _, task := range s.tasks {
		if !taskUnfinished[stringValue(task["status"])] {
			continue
		}
		task["status"] = TaskStatusError
		task["error"] = "服务已重启，未完成的图片任务已中断"
		task["updated_at"] = nowLocal()
		changed = true
	}
	return changed
}

// cleanupLocked 删除超过保留期的终态任务。
func (s *ImageTaskService) cleanupLocked() bool {
	retention := s.cfg.ImageRetentionDays()
	if retention < 1 {
		retention = 30
	}
	cutoff := time.Now().Add(-time.Duration(retention) * 24 * time.Hour).Unix()
	removed := false
	for key, task := range s.tasks {
		if !taskTerminal[stringValue(task["status"])] {
			continue
		}
		if parseTaskTimestamp(stringValue(task["updated_at"])) < float64(cutoff) {
			delete(s.tasks, key)
			removed = true
		}
	}
	return removed
}

// parseTaskTimestamp 复刻 Python 的 _timestamp：失败返回 0。
func parseTaskTimestamp(value string) float64 {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0
	}
	layouts := []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05.000000", "2006-01-02T15:04:05", time.RFC3339Nano}
	for _, layout := range layouts {
		if parsed, err := time.ParseInLocation(layout, trimmed, time.Local); err == nil {
			return float64(parsed.UnixNano()) / 1e9
		}
	}
	return 0
}

func taskKey(ownerID, taskID string) string { return ownerID + ":" + taskID }

func nowLocal() string { return time.Now().Format("2006-01-02 15:04:05") }

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// PublicTask 复刻 _public_task 的字段裁剪规则。
func publicTask(task map[string]any) map[string]any {
	item := map[string]any{
		"id":         task["id"],
		"status":     task["status"],
		"mode":       task["mode"],
		"model":      task["model"],
		"size":       task["size"],
		"quality":    task["quality"],
		"created_at": task["created_at"],
		"updated_at": task["updated_at"],
	}
	if conversationID := stringValue(task["conversation_id"]); conversationID != "" {
		item["conversation_id"] = conversationID
	}
	if data, ok := task["data"]; ok && data != nil {
		item["data"] = data
	}
	if usage, ok := task["usage"]; ok && usage != nil {
		item["usage"] = usage
	}
	if errText := stringValue(task["error"]); errText != "" {
		item["error"] = errText
	}
	if progress := stringValue(task["progress"]); progress != "" {
		item["progress"] = progress
	}
	if duration, ok := task["duration_ms"]; ok && duration != nil {
		item["duration_ms"] = duration
	}
	status := stringValue(task["status"])
	if status == TaskStatusRunning || status == TaskStatusQueued {
		var base float64
		if status == TaskStatusRunning {
			base = floatValue(task["started_ts"])
		} else {
			base = floatValue(task["created_ts"])
			if base == 0 {
				base = floatValue(task["updated_ts"])
			}
		}
		if base > 0 {
			item["elapsed_secs"] = roundTo1(time.Since(time.Unix(0, int64(base*1e9))).Seconds())
		}
	}
	return item
}

func roundTo1(value float64) float64 {
	return float64(int(value*10+0.5)) / 10
}

// ListTasks 复刻 list_tasks：按 owner 隔离，未指定 ids 时返回该 owner 全部。
func (s *ImageTaskService) ListTasks(ownerID string, taskIDs []string) map[string]any {
	s.mu.Lock()
	changed := s.cleanupLocked()
	if changed {
		s.saveLocked()
	}
	s.mu.Unlock()

	owner := orDefault(ownerID, "anonymous")
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]map[string]any, 0)
	missing := make([]string, 0)
	if len(taskIDs) == 0 {
		for _, task := range s.tasks {
			if stringValue(task["owner_id"]) != owner {
				continue
			}
			items = append(items, publicTask(task))
		}
		sort.SliceStable(items, func(i, j int) bool {
			return stringValue(items[i]["updated_at"]) > stringValue(items[j]["updated_at"])
		})
		return map[string]any{"items": items, "missing_ids": missing}
	}
	for _, taskID := range taskIDs {
		task := s.tasks[taskKey(owner, strings.TrimSpace(taskID))]
		if task == nil {
			missing = append(missing, taskID)
			continue
		}
		items = append(items, publicTask(task))
	}
	return map[string]any{"items": items, "missing_ids": missing}
}

// Submit 创建任务并异步执行。
func (s *ImageTaskService) Submit(
	ownerID, clientTaskID, mode, prompt, model, size, quality, baseURL string,
	images, masks [][3]string,
) (map[string]any, error) {
	taskID := strings.TrimSpace(clientTaskID)
	if taskID == "" {
		return nil, fmt.Errorf("client_task_id is required")
	}
	// 先登记再建任务：关闭中的服务连 queued 状态都不该落盘，
	// 否则重启时 recoverUnfinishedLocked 会把它标成「服务已重启」错误。
	release, ok := s.track()
	if !ok {
		return nil, ErrImageTaskClosed
	}
	owner := orDefault(ownerID, "anonymous")
	key := taskKey(owner, taskID)
	now := nowLocal()

	s.mu.Lock()
	cleaned := s.cleanupLocked()
	if existing := s.tasks[key]; existing != nil {
		if cleaned {
			s.saveLocked()
		}
		s.mu.Unlock()
		release()
		return publicTask(existing), nil
	}
	task := map[string]any{
		"id":         taskID,
		"owner_id":   owner,
		"status":     TaskStatusQueued,
		"mode":       mode,
		"model":      orDefault(model, "gpt-image-2"),
		"size":       strings.TrimSpace(size),
		"quality":    orDefault(quality, "auto"),
		"created_at": now,
		"updated_at": now,
		"created_ts": float64(time.Now().UnixNano()) / 1e9,
	}
	s.tasks[key] = task
	s.saveLocked()
	snapshot := publicTask(task)
	s.mu.Unlock()

	go s.runTask(key, TaskContext{
		TaskID:  taskID,
		Mode:    mode,
		Prompt:  prompt,
		Model:   orDefault(model, "gpt-image-2"),
		Size:    strings.TrimSpace(size),
		Quality: orDefault(quality, "auto"),
		BaseURL: baseURL,
		Images:  images,
		Masks:   masks,
	}, release)
	return snapshot, nil
}

// runTask 是任务 goroutine 的主体。release 由 Submit 的 track() 提供，
// 必须 defer 调用，否则 Close 永远等不到它。
func (s *ImageTaskService) runTask(key string, taskCtx TaskContext, release func()) {
	defer release()
	startedAt := time.Now()

	s.mu.Lock()
	task := s.tasks[key]
	if task == nil {
		s.mu.Unlock()
		return
	}
	task["status"] = TaskStatusRunning
	task["started_ts"] = float64(startedAt.UnixNano()) / 1e9
	task["updated_at"] = nowLocal()
	task["updated_ts"] = float64(time.Now().UnixNano()) / 1e9
	s.saveLocked()
	s.mu.Unlock()

	runner := s.generator
	if taskCtx.Mode == "edit" {
		runner = s.editor
	}
	if runner == nil {
		s.finishTask(key, TaskResult{Error: "image task runner is not configured"}, startedAt)
		return
	}

	taskCtx.Progress = func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		s.mu.Lock()
		if current := s.tasks[key]; current != nil {
			current["progress"] = text
			current["updated_at"] = nowLocal()
			current["updated_ts"] = float64(time.Now().UnixNano()) / 1e9
			s.saveLocked()
		}
		s.mu.Unlock()
	}

	result, err := runner(taskCtx)
	if err != nil {
		result.Error = err.Error()
	}
	s.finishTask(key, result, startedAt)
}

func (s *ImageTaskService) finishTask(key string, result TaskResult, startedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[key]
	if task == nil {
		return
	}
	status := TaskStatusSuccess
	if result.Error != "" {
		status = TaskStatusError
	}
	task["status"] = status
	task["updated_at"] = nowLocal()
	task["updated_ts"] = float64(time.Now().UnixNano()) / 1e9
	task["duration_ms"] = time.Since(startedAt).Milliseconds()
	if result.Error != "" {
		task["error"] = result.Error
	}
	if len(result.Data) > 0 {
		data := make([]any, 0, len(result.Data))
		for _, item := range result.Data {
			data = append(data, item)
		}
		task["data"] = data
	}
	if result.Usage != nil {
		task["usage"] = result.Usage
	}
	if result.ConversationID != "" {
		task["conversation_id"] = result.ConversationID
	}
	s.saveLocked()
}

// UpdateTask 供 resume_poll 之类的路径更新字段。
func (s *ImageTaskService) UpdateTask(ownerID, taskID string, updates map[string]any) map[string]any {
	key := taskKey(orDefault(ownerID, "anonymous"), strings.TrimSpace(taskID))
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[key]
	if task == nil {
		return nil
	}
	for name, value := range updates {
		task[name] = value
	}
	task["updated_at"] = nowLocal()
	task["updated_ts"] = float64(time.Now().UnixNano()) / 1e9
	s.saveLocked()
	return publicTask(task)
}

// GetTask 返回内部 task（供 resume_poll 读 conversation_id）。
func (s *ImageTaskService) GetTask(ownerID, taskID string) map[string]any {
	key := taskKey(orDefault(ownerID, "anonymous"), strings.TrimSpace(taskID))
	s.mu.RLock()
	defer s.mu.RUnlock()
	task := s.tasks[key]
	if task == nil {
		return nil
	}
	return copyMap(task)
}

// HasRunners 报告执行体是否已注入。
//
// 存在的理由是让「忘记调 SetRunners」变成一个可断言的事实：
// runner 为 nil 时任务会静默失败成 error 状态，日志里只有一句
// "image task runner is not configured"，很难定位到是装配漏了。
func (s *ImageTaskService) HasRunners() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generator != nil && s.editor != nil
}

// IsTaskTerminal 报告任务状态是否终态。
func IsTaskTerminal(status string) bool {
	return taskTerminal[status]
}
