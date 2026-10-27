package service

import (
	"fmt"
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

// 保留期边界：保留期当天的图**不能**被删。
//
// 回归点：endDate 原先是 now-retention 当天，而 ListItems 的判据是
// `day > endDate` 才过滤掉，于是保留期当天（day == endDate）的图
// 被当成已过期删掉 —— 实测 retention=30 时恰好 30 天前的图被误删。
func TestCleanupKeepsBoundaryDay(t *testing.T) {
	svc, cfg := newImageStorageFixture(t, "local", "")
	root := cfg.ImagesDir()
	// 保留期以**分钟**表达，这里用小时做边界测试更直观。
	// 默认 1440 分钟 = 1 天。
	if _, err := cfg.Update(map[string]any{"image_retention_minutes": 1440}); err != nil {
		t.Fatalf("set retention: %v", err)
	}

	// 判据是**文件 mtime**（保留期可小于 1 天，路径里的日期不足以表达）。
	// 所以这里必须显式设置 mtime，而不是只把文件放在日期目录里。
	mk := func(hoursAgo int) string {
		when := time.Now().Add(-time.Duration(hoursAgo) * time.Hour)
		rel := fmt.Sprintf("%s/bound-%d.png", when.Format("2006/01/02"), hoursAgo)
		full := filepath.Join(root, rel)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte("png"), 0o644)
		_ = os.Chtimes(full, when, when)
		return full
	}
	// 保留期内（< 24h）保留；超过 24h 删除。
	kept := []string{mk(1), mk(23)}
	gone := []string{mk(25), mk(120)}

	svc.CleanupExpiredImages()

	for _, full := range kept {
		if _, err := os.Stat(full); err != nil {
			t.Errorf("保留期内的图被误删: %s", filepath.Base(full))
		}
	}
	for _, full := range gone {
		if _, err := os.Stat(full); !os.IsNotExist(err) {
			t.Errorf("已过期的图未被删: %s", filepath.Base(full))
		}
	}
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

// 注：`CleanupExpired` 的 dirGone 守卫（目录删不掉时保留记录）没有测试覆盖。
// 构造「目录存在但不可删」在当前环境不可能 —— 容器以 root 运行，
// 目录权限位不生效；chattr +i 在 overlayfs 上不可用。
// 该分支的逻辑本身已在实现里加注释说明，且它是「宁可留记录也不丢线索」的
// 保守方向（错了也只是多点重试，不会丢数据）。
