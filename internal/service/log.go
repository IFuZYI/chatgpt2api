package service

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	LogTypeCall    = "call"
	LogTypeAccount = "account"
)

// InternalResponseKeys 是只在内部传递、绝不能出现在对外响应里的字段。
var InternalResponseKeys = map[string]bool{
	"_account_email":   true,
	"_conversation_id": true,
}

// LogService 复刻 services/log_service.py：append-only 单行 JSON。
//
// 磁盘格式是 UI 的数据源，字段集就是契约：
//
//	{"id":"...","time":"2006-01-02 15:04:05","type":"call","summary":"...","detail":{...}}
type LogService struct {
	mu   sync.Mutex
	path string
}

func NewLogService(path string) (*LogService, error) {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return nil, err
	}
	return &LogService{path: path}, nil
}

func dirOf(path string) string {
	index := strings.LastIndexByte(path, os.PathSeparator)
	if index <= 0 {
		return "."
	}
	return path[:index]
}

// legacyID 复刻 Python 的 _legacy_id：老行没有 id 时用 sha1(line_number:raw) 前 24 位。
func legacyID(rawLine string, lineNumber int) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%d:%s", lineNumber, rawLine)))
	return hex.EncodeToString(sum[:])[:24]
}

func parseLogLine(rawLine string, lineNumber int) map[string]any {
	var item map[string]any
	if err := json.Unmarshal([]byte(rawLine), &item); err != nil || item == nil {
		return nil
	}
	if id, _ := item["id"].(string); strings.TrimSpace(id) == "" {
		item["id"] = legacyID(rawLine, lineNumber)
	}
	return item
}

func serializeLogItem(item map[string]any) (string, error) {
	payload, err := json.Marshal(item)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

// Add 追加一条日志。detail 为空时用 rest 作为 detail，与 Python 的 `detail or data` 一致。
func (s *LogService) Add(logType, summary string, detail map[string]any, rest ...map[string]any) {
	if detail == nil {
		detail = map[string]any{}
		if len(rest) > 0 && rest[0] != nil {
			detail = rest[0]
		}
	}
	item := map[string]any{
		"id":      uuid.NewString(),
		"time":    time.Now().Format("2006-01-02 15:04:05"),
		"type":    logType,
		"summary": summary,
		"detail":  detail,
	}
	line, err := serializeLogItem(item)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.WriteString(line + "\n")
}

// List 从最新往回读，最多 limit 条。filter 与 Python 的 _matches_filters 语义一致：
// 日期比较是字符串前缀（YYYY-MM-DD）。
func (s *LogService) List(logType, startDate, endDate string, limit int) []map[string]any {
	if limit <= 0 {
		limit = 200
	}
	s.mu.Lock()
	lines, err := readAllLines(s.path)
	s.mu.Unlock()
	if err != nil {
		return []map[string]any{}
	}
	items := make([]map[string]any, 0, limit)
	for index := len(lines) - 1; index >= 0; index-- {
		item := parseLogLine(lines[index], index)
		if item == nil {
			continue
		}
		if !matchesLogFilters(item, logType, startDate, endDate) {
			continue
		}
		items = append(items, item)
		if len(items) >= limit {
			break
		}
	}
	return items
}

func matchesLogFilters(item map[string]any, logType, startDate, endDate string) bool {
	rawTime, _ := item["time"].(string)
	day := rawTime
	if len(day) > 10 {
		day = day[:10]
	}
	if logType != "" {
		if itemType, _ := item["type"].(string); itemType != logType {
			return false
		}
	}
	if startDate != "" && day < startDate {
		return false
	}
	if endDate != "" && day > endDate {
		return false
	}
	return true
}

// Delete 按 id 删除，其余行重写。返回删除条数。
func (s *LogService) Delete(ids []string) map[string]any {
	targets := make(map[string]bool, len(ids))
	for _, id := range ids {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			targets[trimmed] = true
		}
	}
	if len(targets) == 0 {
		return map[string]any{"removed": 0}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	lines, err := readAllLines(s.path)
	if err != nil {
		return map[string]any{"removed": 0}
	}
	kept := make([]string, 0, len(lines))
	removed := 0
	for index, rawLine := range lines {
		item := parseLogLine(rawLine, index)
		if item == nil {
			kept = append(kept, rawLine)
			continue
		}
		if id, _ := item["id"].(string); targets[id] {
			removed++
			continue
		}
		if serialized, err := serializeLogItem(item); err == nil {
			kept = append(kept, serialized)
		}
	}
	content := strings.Join(kept, "\n")
	if content != "" {
		content += "\n"
	}
	_ = os.WriteFile(s.path, []byte(content), 0o644)
	return map[string]any{"removed": removed}
}

func readAllLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

// stripInternalResponseFields 递归剔除内部字段。对外响应与日志都要过一遍。
func stripInternalResponseFields(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if InternalResponseKeys[key] {
				continue
			}
			out[key] = stripInternalResponseFields(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = stripInternalResponseFields(item)
		}
		return out
	default:
		return value
	}
}

// StripInternal 是 stripInternalResponseFields 的导出包装，供 api 层复用。
func StripInternal(value any) any { return stripInternalResponseFields(value) }
