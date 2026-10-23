package service

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	payload := []byte("hello backup content")
	encrypted, err := EncryptOpenSSL(payload, "testpass")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !bytes.HasPrefix(encrypted, []byte("Salted__")) {
		t.Fatalf("missing Salted__ header: %q", encrypted[:8])
	}
	decrypted, err := DecryptOpenSSL(encrypted, "testpass")
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(decrypted, payload) {
		t.Fatalf("round trip mismatch: %q", decrypted)
	}
}

// 错误的 passphrase 必须失败（否则加密就是摆设）。
func TestDecryptWrongPassphrase(t *testing.T) {
	encrypted, _ := EncryptOpenSSL([]byte("secret"), "rightpass")
	if _, err := DecryptOpenSSL(encrypted, "wrongpass"); err == nil {
		t.Fatal("wrong passphrase should fail")
	}
}

// 关键兼容性验证：Go 加密的产物必须能被系统 openssl 解开。
// 这是 ADR-0006 的硬要求 —— 用户可能在别处用 openssl 解密备份。
func TestGoEncryptionIsOpenSSLCompatible(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not available")
	}
	payload := []byte("cross-tool compatibility check")
	encrypted, err := EncryptOpenSSL(payload, "sharedpass")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	path := filepath.Join(t.TempDir(), "backup.enc")
	if err := os.WriteFile(path, encrypted, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := exec.Command("openssl", "enc", "-d", "-aes-256-cbc", "-pbkdf2", "-md", "sha256",
		"-pass", "pass:sharedpass", "-in", path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("openssl could not decrypt Go output: %v", err)
	}
	if !bytes.Equal(out, payload) {
		t.Fatalf("openssl output mismatch: %q", out)
	}
}

// 反向兼容：openssl 加密的产物，Go 必须能解开。
func TestGoDecryptsOpenSSLOutput(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not available")
	}
	payload := []byte("from openssl to go")
	cmd := exec.Command("openssl", "enc", "-aes-256-cbc", "-pbkdf2", "-salt", "-md", "sha256",
		"-pass", "pass:sharedpass")
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("openssl encrypt: %v", err)
	}

	decrypted, err := DecryptOpenSSL(out, "sharedpass")
	if err != nil {
		t.Fatalf("decrypt openssl output: %v", err)
	}
	if !bytes.Equal(decrypted, payload) {
		t.Fatalf("mismatch: %q", decrypted)
	}
}

func TestArchiveEncodeDecodeRoundTrip(t *testing.T) {
	archive := &BackupArchive{
		Files: map[string][]byte{
			"config.json": []byte(`{"auth-key":"x"}`),
			"logs.jsonl":  []byte("line1\nline2\n"),
		},
		Snapshots: map[string]any{
			"accounts": map[string]any{"items": []any{map[string]any{"access_token": "t"}}},
		},
		Metadata: map[string]any{"version": 2, "app_version": "1.8.0", "trigger": "manual", "created_at": "2026-10-23T12:00:00Z"},
	}

	encoded, err := archive.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	detail, err := DecodeArchive(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	files, _ := detail["files"].([]map[string]any)
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2", len(files))
	}
	// 排序后 config.json 在 logs.jsonl 之前
	if stringValue(files[0]["name"]) != "config.json" {
		t.Errorf("files not sorted: %v", files[0]["name"])
	}
	// sha256 必须是 64 位 hex
	sha, _ := files[0]["sha256"].(string)
	if len(sha) != 64 {
		t.Errorf("sha256 length = %d", len(sha))
	}

	snapshots, _ := detail["snapshots"].([]map[string]any)
	if len(snapshots) != 1 || stringValue(snapshots[0]["name"]) != "accounts" {
		t.Fatalf("snapshots = %v", snapshots)
	}
	if snapshots[0]["count"] != 1 {
		t.Errorf("snapshot count = %v, want 1", snapshots[0]["count"])
	}

	// metadata 必须摊平到顶层：前端读 detail.created_at / detail.trigger /
	// detail.app_version / detail.storage_backend（不是 detail.metadata.*）。
	// 早期实现返回嵌套的 metadata，导致备份详情页四个字段全部显示为空。
	if detail["app_version"] != "1.8.0" {
		t.Errorf("app_version not flattened: %v", detail["app_version"])
	}
	if detail["trigger"] != "manual" {
		t.Errorf("trigger not flattened: %v", detail["trigger"])
	}
	if detail["created_at"] == nil {
		t.Error("created_at should be present at top level")
	}
	if _, ok := detail["storage_backend"]; !ok {
		t.Error("storage_backend should be present (empty map when unknown)")
	}
	// 摊平后不应再暴露嵌套的 metadata 键
	if _, ok := detail["metadata"]; ok {
		t.Error("metadata should be flattened, not nested")
	}
}

func TestBackupObjectKey(t *testing.T) {
	ts := time.Date(2026, 10, 23, 12, 34, 56, 0, time.UTC)
	key := BackupObjectKey("backups", ts, "ab12", false)
	if key != "backups/backup-20261023T123456Z-ab12.tar.gz" {
		t.Errorf("key = %q", key)
	}
	encryptedKey := BackupObjectKey("custom/prefix", ts, "cd34", true)
	if encryptedKey != "custom/prefix/backup-20261023T123456Z-cd34.tar.gz.enc" {
		t.Errorf("encrypted key = %q", encryptedKey)
	}
	// 空 prefix 回退到 backups
	if key := BackupObjectKey("", ts, "0000", false); key[:8] != "backups/" {
		t.Errorf("empty prefix should default to backups: %q", key)
	}
}

func TestDecodeArchiveRejectsGarbage(t *testing.T) {
	if _, err := DecodeArchive([]byte("not a gzip stream")); err == nil {
		t.Fatal("garbage should fail to decode")
	}
}
