package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// D-1 回归：保留清理必须真的删文件。
//
// 修复前的症状：CleanupExpiredImages 传 allMatching=false 且 paths=nil，
// DeleteImages 的 targets 为空 → removed 恒为 0，清理完全空转。
func TestCleanupExpiredImagesReallyDeletes(t *testing.T) {
	svc, cfg := newImageStorageFixture(t, "local", "")
	root := cfg.ImagesDir()
	rel := "2020/01/01/old.png"
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	_ = os.Chtimes(full, old, old)

	// 默认保留 30 天，2020 年的图必然过期，无需改配置。
	removed := svc.CleanupExpiredImages()
	if removed == 0 {
		t.Fatal("保留清理 removed=0 —— 过期图应被删除")
	}
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Fatal("文件仍在：保留清理没有真正执行")
	}
	t.Logf("removed=%d，文件已删除", removed)
}

// 未过期的图不能被删。
func TestCleanupKeepsFreshImages(t *testing.T) {
	svc, cfg := newImageStorageFixture(t, "local", "")
	rel := time.Now().Format("2006/01/02") + "/fresh.png"
	full := filepath.Join(cfg.ImagesDir(), rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if removed := svc.CleanupExpiredImages(); removed != 0 {
		t.Errorf("新鲜图被删了：removed=%d", removed)
	}
	if _, err := os.Stat(full); err != nil {
		t.Fatal("新鲜图不见了")
	}
}

// D-2 回归：可编辑产物目录必须真的被删，且记录字段名是 id。
func TestCleanupEditableReallyDeletesDir(t *testing.T) {
	svc, dataDir := newEditableTestServiceWithDir(t)
	taskID := "del-me-1"
	dir := filepath.Join(dataDir, "files", "ppt", taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "deck.pptx"), []byte("x"), 0o644)

	svc.mu.Lock()
	svc.tasks["u:"+taskID] = map[string]any{
		"id": taskID, "kind": "ppt", "status": TaskStatusSuccess,
		"updated_at": time.Now().Add(-90 * 24 * time.Hour).Format(accountTimeLayout),
	}
	svc.mu.Unlock()

	tasks, dirs := svc.CleanupExpired(30)
	if tasks == 0 {
		t.Fatal("任务记录未清理")
	}
	if dirs == 0 {
		t.Fatal("产物目录未清理 —— 字段名应为 id")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("产物目录仍在磁盘上")
	}
	t.Logf("tasksRemoved=%d dirsRemoved=%d，目录已删除", tasks, dirs)
}
