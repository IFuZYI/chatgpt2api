package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/config"
)

// 符号链接逃逸：data/images/ 下指向外部文件的软链必须被拒绝。
//
// 回归点：filepath.Abs 只做字符串 Clean，不跟随 symlink，
// 于是字面路径仍在 root 下的软链能通过前缀检查，
// 未鉴权的 GET /images/<name> 就能读出 config.json（含 auth-key）。已实测复现。
func TestLocalImagePathRejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auth-key":"secret"}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("2.0.0"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	cfg, err := config.NewStore(dir)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	images := NewImageService(cfg)
	if err := os.MkdirAll(cfg.ImagesDir(), 0o755); err != nil {
		t.Fatalf("mkdir images: %v", err)
	}

	// 正常文件应通过
	normal := filepath.Join(cfg.ImagesDir(), "ok.png")
	if err := os.WriteFile(normal, []byte("PNG"), 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}
	if _, err := images.localImagePath("ok.png"); err != nil {
		t.Errorf("normal file should resolve, got: %v", err)
	}

	// 指向 config.json 的软链必须被拒
	evil := filepath.Join(cfg.ImagesDir(), "evil.png")
	if err := os.Symlink(filepath.Join(dir, "config.json"), evil); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	if _, err := images.localImagePath("evil.png"); err == nil {
		t.Fatal("symlink pointing outside images dir must be rejected")
	}
}
