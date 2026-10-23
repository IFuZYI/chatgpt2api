package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// backupStateStore 维护备份运行状态。
//
// 两件事：
//   - 落盘 data/backup_state.json（last_status / last_finished_at / last_object_key / last_error）
//   - 内存态 running（进程内的「正在备份」标志，不落盘）
//
// 为什么必须有：前端备份卡片的「备份中」徽标、按钮禁用、历史状态栏
// 全部依赖这两个来源。缺了它们，状态栏永久空白，并发点「立即备份」
// 也不会被拦住（Python 靠 _running 返回「当前已有备份任务正在执行」）。
type backupStateStore struct {
	mu      sync.Mutex
	path    string
	running bool
}

func newBackupStateStore(path string) *backupStateStore {
	return &backupStateStore{path: path}
}

// Begin 尝试开始一次备份。已有任务在跑时返回 false。
func (s *backupStateStore) Begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	return true
}

// End 结束一次备份并记录结果。
func (s *backupStateStore) End(status string, objectKey, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false

	current := s.loadLocked()
	next := map[string]any{
		"last_started_at":  current["last_started_at"],
		"last_finished_at": nowISO(),
		"last_status":      status,
		"last_error":       nilIfEmpty(errText),
		"last_object_key":  nilIfEmpty(objectKey),
	}
	s.saveLocked(next)
}

// MarkStarted 记录一次备份的开始时间（保留上一次的完成信息）。
func (s *backupStateStore) MarkStarted() {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.loadLocked()
	s.saveLocked(map[string]any{
		"last_started_at":  nowISO(),
		"last_finished_at": current["last_finished_at"],
		"last_status":      "idle",
		"last_error":       nil,
		"last_object_key":  current["last_object_key"],
	})
}

// Snapshot 返回前端读的状态（磁盘内容 + running）。
func (s *backupStateStore) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	state := s.loadLocked()
	state["running"] = s.running
	return state
}

// IsRunning 报告是否有备份在跑。
func (s *backupStateStore) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *backupStateStore) loadLocked() map[string]any {
	empty := map[string]any{
		"last_started_at":  nil,
		"last_finished_at": nil,
		"last_status":      "idle",
		"last_error":       nil,
		"last_object_key":  nil,
	}
	payload, err := os.ReadFile(s.path)
	if err != nil {
		return empty
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return empty
	}
	// 补齐缺失键，避免前端读到 undefined。
	for key, value := range empty {
		if _, ok := decoded[key]; !ok {
			decoded[key] = value
		}
	}
	return decoded
}

func (s *backupStateStore) saveLocked(state map[string]any) {
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	payload = append(payload, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(s.path, payload, 0o644)
}

// nilIfEmpty 空字符串转 nil（JSON 输出 null 而非 ""）。
func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
