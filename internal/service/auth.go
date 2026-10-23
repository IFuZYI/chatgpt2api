package service

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/storage"
	"github.com/google/uuid"
)

// AuthRole 只有两个取值，与 Python 的 Literal["admin","user"] 对应。
type AuthRole string

const (
	RoleAdmin AuthRole = "admin"
	RoleUser  AuthRole = "user"
)

// 认证相关错误。文案是对外契约（前端直接展示），逐字保留。
var (
	ErrAuthKeyEmpty      = errors.New("请输入新的专用密钥")
	ErrAuthKeyConflict   = errors.New("这个密钥和管理员密钥冲突了，请换一个新的密钥")
	ErrAuthKeyDuplicate  = errors.New("这个专用密钥已经存在，请换一个新的密钥")
	ErrAuthNameDuplicate = errors.New("这个名称已经在使用中了，换一个更容易区分的名称吧")
)

// AuthService 复刻 services/auth_service.py。
//
// 哈希是裸 SHA-256 hex（无盐），这是刻意的：换成更强的哈希会让存量
// data/auth_keys.json 全部失效（见 ADR-0006）。比较用 hmac.Equal 对齐
// Python 的 hmac.compare_digest，避免时序侧信道。
type AuthService struct {
	mu       sync.Mutex
	storage  storage.Backend
	items    []map[string]any
	adminKey func() string

	lastUsedFlushAt map[string]time.Time
}

func NewAuthService(backend storage.Backend, adminKey func() string) *AuthService {
	s := &AuthService{
		storage:         backend,
		adminKey:        adminKey,
		lastUsedFlushAt: map[string]time.Time{},
	}
	s.items = s.load()
	return s
}

func hashKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func defaultNameFor(role string) string {
	if strings.ToLower(strings.TrimSpace(role)) == "admin" {
		return "管理员密钥"
	}
	return "普通用户"
}

func cleanString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func (s *AuthService) normalizeItem(raw map[string]any) map[string]any {
	if raw == nil {
		return nil
	}
	role := strings.ToLower(cleanString(raw["role"]))
	if role != "admin" && role != "user" {
		return nil
	}
	keyHash := cleanString(raw["key_hash"])
	if keyHash == "" {
		return nil
	}
	itemID := cleanString(raw["id"])
	if itemID == "" {
		itemID = uuid.NewString()[:12]
	}
	name := cleanString(raw["name"])
	if name == "" {
		name = defaultNameFor(role)
	}
	createdAt := cleanString(raw["created_at"])
	if createdAt == "" {
		createdAt = nowISO()
	}
	lastUsedAt := cleanString(raw["last_used_at"])
	enabled := true
	if value, ok := raw["enabled"].(bool); ok {
		enabled = value
	} else if raw["enabled"] != nil {
		enabled = true
	}
	var lastUsed any
	if lastUsedAt != "" {
		lastUsed = lastUsedAt
	}
	return map[string]any{
		"id":           itemID,
		"name":         name,
		"role":         role,
		"key_hash":     keyHash,
		"enabled":      enabled,
		"created_at":   createdAt,
		"last_used_at": lastUsed,
	}
}

func (s *AuthService) load() []map[string]any {
	raw, err := s.storage.LoadAuthKeys()
	if err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if normalized := s.normalizeItem(item); normalized != nil {
			out = append(out, normalized)
		}
	}
	return out
}

func (s *AuthService) save() error {
	return s.storage.SaveAuthKeys(s.items)
}

func publicAuthItem(item map[string]any) map[string]any {
	enabled := true
	if value, ok := item["enabled"].(bool); ok {
		enabled = value
	}
	return map[string]any{
		"id":           item["id"],
		"name":         item["name"],
		"role":         item["role"],
		"enabled":      enabled,
		"created_at":   item["created_at"],
		"last_used_at": item["last_used_at"],
	}
}

// ListKeys 每次调用都从存储重载，与 Python 一致（多进程/外部编辑可见）。
//
// 关键：必须在持锁期间完成对 items 的遍历与拷贝。
// 早期实现把 slice 取出来就解锁、然后在锁外遍历，
// 而 Authenticate 会在锁内替换 s.items[index] ——
// 两者并发时构成数据竞争（go test -race 稳定复现）。
func (s *AuthService) ListKeys(role AuthRole) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.items = s.load()
	out := make([]map[string]any, 0, len(s.items))
	for _, item := range s.items {
		if role != "" && cleanString(item["role"]) != string(role) {
			continue
		}
		out = append(out, publicAuthItem(item))
	}
	return out
}

func (s *AuthService) hasKeyHashLocked(keyHash, excludeID string) bool {
	for _, item := range s.items {
		itemID := cleanString(item["id"])
		if excludeID != "" && itemID == excludeID {
			continue
		}
		stored := cleanString(item["key_hash"])
		if stored != "" && hmac.Equal([]byte(stored), []byte(keyHash)) {
			return true
		}
	}
	return false
}

func (s *AuthService) buildKeyHashLocked(rawKey, excludeID string) (string, error) {
	candidate := strings.TrimSpace(rawKey)
	if candidate == "" {
		return "", ErrAuthKeyEmpty
	}
	if admin := strings.TrimSpace(s.adminKey()); admin != "" && hmac.Equal([]byte(candidate), []byte(admin)) {
		return "", ErrAuthKeyConflict
	}
	keyHash := hashKey(candidate)
	if s.hasKeyHashLocked(keyHash, excludeID) {
		return "", ErrAuthKeyDuplicate
	}
	return keyHash, nil
}

func (s *AuthService) hasNameLocked(name string, role AuthRole, excludeID string) bool {
	candidate := strings.TrimSpace(name)
	if candidate == "" {
		return false
	}
	for _, item := range s.items {
		itemID := cleanString(item["id"])
		if excludeID != "" && itemID == excludeID {
			continue
		}
		if role != "" && cleanString(item["role"]) != string(role) {
			continue
		}
		if cleanString(item["name"]) == candidate {
			return true
		}
	}
	return false
}

func (s *AuthService) buildDefaultNameLocked(role AuthRole, excludeID string) string {
	baseName := defaultNameFor(string(role))
	if !s.hasNameLocked(baseName, role, excludeID) {
		return baseName
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s %d", baseName, suffix)
		if !s.hasNameLocked(candidate, role, excludeID) {
			return candidate
		}
	}
}

func (s *AuthService) buildNameLocked(name string, role AuthRole, excludeID string) (string, error) {
	candidate := strings.TrimSpace(name)
	if candidate == "" {
		return s.buildDefaultNameLocked(role, excludeID), nil
	}
	if s.hasNameLocked(candidate, role, excludeID) {
		return "", ErrAuthNameDuplicate
	}
	return candidate, nil
}

// randomKey 生成 sk-<token_urlsafe(24)> 形态的密钥。
func randomKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "sk-" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// CreateKey 返回 (公开条目, 明文密钥)。
func (s *AuthService) CreateKey(role AuthRole, name string) (map[string]any, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = s.load()

	normalizedName, err := s.buildNameLocked(name, role, "")
	if err != nil {
		return nil, "", err
	}
	var rawKey, keyHash string
	for attempt := 0; attempt < 100; attempt++ {
		candidate, err := randomKey()
		if err != nil {
			return nil, "", err
		}
		hash, err := s.buildKeyHashLocked(candidate, "")
		if err != nil {
			continue
		}
		rawKey, keyHash = candidate, hash
		break
	}
	if rawKey == "" {
		return nil, "", errors.New("failed to generate a unique key")
	}
	item := map[string]any{
		"id":           uuid.NewString()[:12],
		"name":         normalizedName,
		"role":         string(role),
		"key_hash":     keyHash,
		"enabled":      true,
		"created_at":   nowISO(),
		"last_used_at": nil,
	}
	s.items = append(s.items, item)
	if err := s.save(); err != nil {
		return nil, "", err
	}
	return publicAuthItem(item), rawKey, nil
}

// UpdateKey 的 updates 用 nil 表示「未提供」，与 Pydantic 的 Optional 语义一致。
func (s *AuthService) UpdateKey(keyID string, updates map[string]any, role AuthRole) (map[string]any, error) {
	normalizedID := strings.TrimSpace(keyID)
	if normalizedID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = s.load()

	for index, item := range s.items {
		if cleanString(item["id"]) != normalizedID {
			continue
		}
		if role != "" && cleanString(item["role"]) != string(role) {
			return nil, nil
		}
		nextItem := copyMap(item)
		nextRole := RoleUser
		if strings.ToLower(cleanString(nextItem["role"])) == "admin" {
			nextRole = RoleAdmin
		}
		if value, ok := updates["name"]; ok && value != nil {
			built, err := s.buildNameLocked(cleanString(value), nextRole, normalizedID)
			if err != nil {
				return nil, err
			}
			nextItem["name"] = built
		}
		if value, ok := updates["enabled"]; ok && value != nil {
			if flag, ok := value.(bool); ok {
				nextItem["enabled"] = flag
			} else {
				nextItem["enabled"] = truthy(value)
			}
		}
		if value, ok := updates["key"]; ok && value != nil {
			built, err := s.buildKeyHashLocked(cleanString(value), normalizedID)
			if err != nil {
				return nil, err
			}
			nextItem["key_hash"] = built
		}
		s.items[index] = nextItem
		if err := s.save(); err != nil {
			return nil, err
		}
		return publicAuthItem(nextItem), nil
	}
	return nil, nil
}

func (s *AuthService) DeleteKey(keyID string, role AuthRole) bool {
	normalizedID := strings.TrimSpace(keyID)
	if normalizedID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = s.load()

	before := len(s.items)
	kept := make([]map[string]any, 0, len(s.items))
	for _, item := range s.items {
		matches := cleanString(item["id"]) == normalizedID
		if role != "" && cleanString(item["role"]) != string(role) {
			matches = false
		}
		if matches {
			continue
		}
		kept = append(kept, item)
	}
	if len(kept) == before {
		return false
	}
	s.items = kept
	_ = s.save()
	return true
}

// Authenticate 返回身份；未命中返回 nil。
// last_used_at 最多每 60 秒落盘一次，避免每个请求都写文件。
func (s *AuthService) Authenticate(rawKey string) map[string]any {
	candidate := strings.TrimSpace(rawKey)
	if candidate == "" {
		return nil
	}
	candidateHash := hashKey(candidate)

	s.mu.Lock()
	defer s.mu.Unlock()

	for index, item := range s.items {
		if enabled, ok := item["enabled"].(bool); ok && !enabled {
			continue
		}
		stored := cleanString(item["key_hash"])
		if stored == "" || !hmac.Equal([]byte(stored), []byte(candidateHash)) {
			continue
		}
		nextItem := copyMap(item)
		now := time.Now().UTC()
		nextItem["last_used_at"] = now.Format(time.RFC3339Nano)
		s.items[index] = nextItem
		itemID := cleanString(nextItem["id"])
		lastFlush, seen := s.lastUsedFlushAt[itemID]
		if !seen || now.Sub(lastFlush) >= 60*time.Second {
			if err := s.save(); err == nil {
				s.lastUsedFlushAt[itemID] = now
			}
		}
		return publicAuthItem(nextItem)
	}
	return nil
}

func copyMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	case float64:
		return typed != 0
	default:
		return false
	}
}

// RawKeysForBackup 返回含 key_hash 的原始密钥列表。
//
// 仅供备份使用：备份的用途就是恢复，脱敏后的 public 形状（无 hash）
// 恢复出来是一堆无法鉴权的空壳。Python 备份直接存
// storage.load_auth_keys() 的原始内容，形状一致。
func (s *AuthService) RawKeysForBackup() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.items = s.load()
	out := make([]map[string]any, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, copyMap(item))
	}
	return out
}
