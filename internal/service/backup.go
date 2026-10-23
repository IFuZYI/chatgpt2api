package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

// BackupError 是备份链路的业务错误（对外展示文案）。
type BackupError struct{ Message string }

func (e *BackupError) Error() string { return e.Message }

func newBackupError(format string, args ...any) *BackupError {
	return &BackupError{Message: fmt.Sprintf(format, args...)}
}

// 备份归档内的固定文件名。
const (
	backupMetadataName = "backup-metadata.json"
	backupSnapshotDir  = "snapshots"
	// opensslPBKDF2Iterations 是 OpenSSL 3.x `enc -pbkdf2` 的默认迭代次数。
	// 实测确认：10000 能解出，100000 不行（见 ADR-0006 的验证记录）。
	opensslPBKDF2Iterations = 10000
)

// BackupArchive 是备份内容的内存表示。
type BackupArchive struct {
	Files     map[string][]byte
	Snapshots map[string]any
	Metadata  map[string]any
}

// BackupSettings 是备份配置的运行时快照。
type BackupSettings struct {
	Enabled      bool
	AccountID    string
	AccessKeyID  string
	SecretKey    string
	Bucket       string
	Prefix       string
	IntervalMin  int
	RotationKeep int
	Encrypt      bool
	Passphrase   string
	Include      map[string]any
}

// BuildBackupArchive 复刻 _build_backup_archive：
// 按 include 配置收集 config / cpa / sub2api / logs / image_tasks / 账号快照 / 密钥快照。
func (s *BackupBuilder) Build(settings BackupSettings, trigger string) (*BackupArchive, error) {
	include := settings.Include
	archive := &BackupArchive{
		Files:     map[string][]byte{},
		Snapshots: map[string]any{},
	}

	addFile := func(name, sourcePath string) {
		if !fileExists(sourcePath) {
			return
		}
		payload, err := os.ReadFile(sourcePath)
		if err != nil {
			return
		}
		archive.Files[name] = payload
	}

	if truthy(include["config"]) {
		addFile("config.json", s.ConfigPath)
	}
	if truthy(include["cpa"]) {
		addFile("cpa_config.json", filepath.Join(s.DataDir, "cpa_config.json"))
	}
	if truthy(include["sub2api"]) {
		addFile("sub2api_config.json", filepath.Join(s.DataDir, "sub2api_config.json"))
	}
	// 归档内路径带 "data/" 前缀：Python _add_file_to_archive(archive, LOGS_FILE, "data/logs.jsonl")。
	// 少了前缀，用户按官方恢复步骤解包会把文件放错目录。
	if truthy(include["logs"]) {
		addFile("data/logs.jsonl", filepath.Join(s.DataDir, "logs.jsonl"))
	}
	if truthy(include["image_tasks"]) {
		addFile("data/image_tasks.json", filepath.Join(s.DataDir, "image_tasks.json"))
	}
	if truthy(include["images"]) {
		addFile("data/image_tags.json", filepath.Join(s.DataDir, "image_tags.json"))
		addFile("data/image_index.json", filepath.Join(s.DataDir, "image_index.json"))
	}

	// 快照形状必须与 Python 一致：snapshots/accounts.json 与
	// snapshots/auth_keys.json 存的是**裸数组**（load_accounts/load_auth_keys
	// 的原始返回值），不是 {count,items} 包装。
	//
	// 而且 auth key 必须保留 key_hash —— ListKeys 返回的是脱敏后的
	// public 形状（无 hash），拿它做备份等于备份了一堆不可用的密钥。
	if truthy(include["accounts_snapshot"]) && s.Accounts != nil {
		archive.Snapshots["accounts"] = s.Accounts.ListAccounts()
	}
	if truthy(include["auth_keys_snapshot"]) && s.Auth != nil {
		archive.Snapshots["auth_keys"] = s.Auth.RawKeysForBackup()
	}

	// metadata 的键与 Python _build_backup_archive 逐字对齐：
	//   version        = 2（归档格式版本，不是应用版本）
	//   app_version    = 应用版本号
	//   storage_backend= 存储后端信息
	// 早期实现把 version 填成应用版本、且缺 app_version/storage_backend，
	// 导致备份详情页的「应用版本」「存储后端」两栏永远是空。
	archive.Metadata = map[string]any{
		"version":         2,
		"created_at":      time.Now().UTC().Format(time.RFC3339Nano),
		"trigger":         trigger,
		"app_version":     s.Version,
		"storage_backend": s.StorageBackendInfo,
		"include":         include,
	}
	return archive, nil
}

// BackupBuilder 持有构建归档所需的依赖。
type BackupBuilder struct {
	ConfigPath string
	DataDir    string
	Version    string
	// StorageBackendInfo 由调用方注入（storage.Backend.BackendInfo()）。
	StorageBackendInfo map[string]any
	Accounts           interface{ ListAccounts() []Account }
	Auth               interface {
		ListKeys(role AuthRole) []map[string]any
		// RawKeysForBackup 返回含 key_hash 的原始密钥（备份需要可恢复）。
		RawKeysForBackup() []map[string]any
	}
}

// Encode 把归档打成 tar.gz（未加密）。
func (a *BackupArchive) Encode() ([]byte, error) {
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)

	// 固定顺序，保证同样的内容产出同样的字节（便于校验）。
	names := make([]string, 0, len(a.Files))
	for name := range a.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := writeTarFile(tarWriter, name, a.Files[name]); err != nil {
			return nil, err
		}
	}

	metadataPayload, err := json.MarshalIndent(a.Metadata, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeTarFile(tarWriter, backupMetadataName, append(metadataPayload, '\n')); err != nil {
		return nil, err
	}

	snapshotNames := make([]string, 0, len(a.Snapshots))
	for name := range a.Snapshots {
		snapshotNames = append(snapshotNames, name)
	}
	sort.Strings(snapshotNames)
	for _, name := range snapshotNames {
		payload, err := json.MarshalIndent(a.Snapshots[name], "", "  ")
		if err != nil {
			return nil, err
		}
		if err := writeTarFile(tarWriter, backupSnapshotDir+"/"+name+".json", append(payload, '\n')); err != nil {
			return nil, err
		}
	}

	if err := tarWriter.Close(); err != nil {
		return nil, err
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeTarFile(writer *tar.Writer, name string, payload []byte) error {
	header := &tar.Header{
		Name:    name,
		Mode:    0o644,
		Size:    int64(len(payload)),
		ModTime: time.Now(),
	}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	_, err := writer.Write(payload)
	return err
}

// DecodeArchive 复刻 _decode_archive_detail：解析 tar.gz 并列出内容。
func DecodeArchive(payload []byte) (map[string]any, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, newBackupError("解析备份压缩包失败，备份可能已损坏")
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	files := make([]map[string]any, 0, 8)
	snapshots := make([]map[string]any, 0, 4)
	metadata := map[string]any{}

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, newBackupError("解析备份压缩包失败，备份可能已损坏")
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tarReader, 256*1024*1024))
		if err != nil {
			return nil, newBackupError("读取备份条目失败")
		}
		name := header.Name

		if name == backupMetadataName {
			var parsed map[string]any
			if json.Unmarshal(raw, &parsed) == nil && parsed != nil {
				metadata = parsed
			}
			continue
		}
		if strings.HasPrefix(name, backupSnapshotDir+"/") && strings.HasSuffix(name, ".json") {
			count := 0
			var parsed any
			if json.Unmarshal(raw, &parsed) == nil {
				count = countItems(parsed)
			}
			snapshots = append(snapshots, map[string]any{
				"name":  strings.TrimSuffix(strings.TrimPrefix(name, backupSnapshotDir+"/"), ".json"),
				"count": count,
			})
			continue
		}

		sum := sha256.Sum256(raw)
		files = append(files, map[string]any{
			"name":         name,
			"exists":       true,
			"content_type": guessContentType(name),
			"size":         len(raw),
			"sha256":       hex.EncodeToString(sum[:]),
		})
	}

	sort.SliceStable(files, func(i, j int) bool {
		return stringValue(files[i]["name"]) < stringValue(files[j]["name"])
	})
	// 摊平 metadata 到顶层：前端读 detail.created_at / detail.trigger /
	// detail.app_version / detail.storage_backend（不是 detail.metadata.*）。
	// Python _decode_archive_detail 也是摊平的。
	out := map[string]any{
		"files":           files,
		"snapshots":       snapshots,
		"created_at":      metadata["created_at"],
		"trigger":         metadata["trigger"],
		"app_version":     metadata["app_version"],
		"storage_backend": metadata["storage_backend"],
	}
	if out["storage_backend"] == nil {
		out["storage_backend"] = map[string]any{}
	}
	return out, nil
}

// countItems 复刻 _count_items：数 items / data / accounts 数组长度。
func countItems(value any) int {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"items", "data", "accounts", "keys"} {
			if items, ok := typed[key].([]any); ok {
				return len(items)
			}
		}
		if count := intValue(typed["count"]); count > 0 {
			return count
		}
		return 0
	case []any:
		return len(typed)
	default:
		return 0
	}
}

func guessContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".jsonl"):
		return "application/x-ndjson"
	case strings.HasSuffix(name, ".log"), strings.HasSuffix(name, ".txt"):
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

// EncryptOpenSSL 用 AES-256-CBC + PBKDF2-SHA256 加密，输出 OpenSSL "Salted__" 格式。
//
// 这是刻意的兼容实现：Python 版 shell out 到 `openssl enc`，Go 版用原生 crypto，
// 但产物必须能被 `openssl enc -d` 解开（ADR-0006）。格式：
//
//	"Salt__" (8 bytes) + salt (8 bytes) + ciphertext (PKCS#7 padded)
//
// 注意头是字面量 "Salted__"。
func EncryptOpenSSL(payload []byte, passphrase string) ([]byte, error) {
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	keyIV := pbkdf2.Key([]byte(passphrase), salt, opensslPBKDF2Iterations, 48, sha256.New)
	key, iv := keyIV[:32], keyIV[32:]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padded := pkcs7Pad(payload, aes.BlockSize)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)

	out := make([]byte, 0, 16+len(ciphertext))
	out = append(out, []byte("Salted__")...)
	out = append(out, salt...)
	out = append(out, ciphertext...)
	return out, nil
}

// DecryptOpenSSL 解密 EncryptOpenSSL 的产物。
func DecryptOpenSSL(payload []byte, passphrase string) ([]byte, error) {
	if len(payload) < 16 || string(payload[:8]) != "Salted__" {
		return nil, newBackupError("解密备份失败：文件不是 OpenSSL 加密格式")
	}
	salt := payload[8:16]
	ciphertext := payload[16:]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, newBackupError("解密备份失败：密文长度非法")
	}

	keyIV := pbkdf2.Key([]byte(passphrase), salt, opensslPBKDF2Iterations, 48, sha256.New)
	key, iv := keyIV[:32], keyIV[32:]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)
	return pkcs7Unpad(plaintext, aes.BlockSize)
}

func pkcs7Pad(payload []byte, blockSize int) []byte {
	padding := blockSize - len(payload)%blockSize
	out := make([]byte, len(payload)+padding)
	copy(out, payload)
	for index := len(payload); index < len(out); index++ {
		out[index] = byte(padding)
	}
	return out
}

func pkcs7Unpad(payload []byte, blockSize int) ([]byte, error) {
	if len(payload) == 0 || len(payload)%blockSize != 0 {
		return nil, newBackupError("解密备份失败：填充非法")
	}
	padding := int(payload[len(payload)-1])
	if padding == 0 || padding > blockSize || padding > len(payload) {
		return nil, newBackupError("解密备份失败：填充非法")
	}
	for index := len(payload) - padding; index < len(payload); index++ {
		if int(payload[index]) != padding {
			return nil, newBackupError("解密备份失败：填充非法")
		}
	}
	return payload[:len(payload)-padding], nil
}

// BackupObjectKey 复刻对象键生成：<prefix>/backup-<ts>-<tag><suffix>。
func BackupObjectKey(prefix string, timestamp time.Time, tag string, encrypted bool) string {
	suffix := ".tar.gz"
	if encrypted {
		suffix = ".tar.gz.enc"
	}
	cleanPrefix := strings.Trim(strings.TrimSpace(prefix), "/")
	if cleanPrefix == "" {
		cleanPrefix = "backups"
	}
	return fmt.Sprintf("%s/backup-%s-%s%s",
		cleanPrefix, timestamp.UTC().Format("20060102T150405Z"), tag, suffix)
}

// RandomHexTag 生成 4 位十六进制 tag（Python 用 random.randint(0, 0xFFFF)）。
func RandomHexTag() string {
	buf := make([]byte, 2)
	if _, err := rand.Read(buf); err != nil {
		return "0000"
	}
	return hex.EncodeToString(buf)
}
