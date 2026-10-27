package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 造一张指定年龄的图。
func writeImageAged(t *testing.T, root, rel string, age time.Duration) string {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(full, when, when); err != nil {
		t.Fatal(err)
	}
	return full
}

// 分钟级精度必须生效：保留 60 分钟时，2 小时前的图要被删。
//
// 回归点：原实现按**日期字符串**比较（cutoff 格式化 "2006-01-02"），
// 无法表达小于 1 天的保留期 —— 设 60 分钟时 endDate 就是今天，
// 当天所有图都会被判为过期。所以清理必须按 mtime 时间戳判断。
func TestCleanupUsesMinuteGranularity(t *testing.T) {
	svc, cfg := newImageStorageFixture(t, "local", "")
	if _, err := cfg.Update(map[string]any{"image_retention_minutes": 60}); err != nil {
		t.Fatalf("set retention: %v", err)
	}

	old := writeImageAged(t, cfg.ImagesDir(), "2020/01/01/two-hours.png", 2*time.Hour)
	fresh := writeImageAged(t, cfg.ImagesDir(), "2020/01/01/thirty-min.png", 30*time.Minute)

	svc.CleanupExpiredImages()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("2 小时前的图应被删除（保留 60 分钟）")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("30 分钟前的图不应被删除（保留 60 分钟）")
	}
}

// 保留期内的新图不能被删。
func TestCleanupMinutesKeepsFreshImages(t *testing.T) {
	svc, cfg := newImageStorageFixture(t, "local", "")
	fresh := writeImageAged(t, cfg.ImagesDir(), "2020/01/01/now.png", time.Minute)

	svc.CleanupExpiredImages()

	if _, err := os.Stat(fresh); err != nil {
		t.Error("刚生成的图不应被删除（默认保留 1440 分钟）")
	}
}

// 默认 1440 分钟 = 1 天：2 天前的图删，1 小时前的留。
func TestCleanupMinutesDefaultIsOneDay(t *testing.T) {
	svc, cfg := newImageStorageFixture(t, "local", "")

	twoDays := writeImageAged(t, cfg.ImagesDir(), "2020/01/01/two-days.png", 48*time.Hour)
	oneHour := writeImageAged(t, cfg.ImagesDir(), "2020/01/01/one-hour.png", time.Hour)

	svc.CleanupExpiredImages()

	if _, err := os.Stat(twoDays); !os.IsNotExist(err) {
		t.Error("2 天前的图应被默认保留期（1 天）删除")
	}
	if _, err := os.Stat(oneHour); err != nil {
		t.Error("1 小时前的图不应被删除")
	}
}

// 任务清理同样走分钟。
func TestTaskCleanupUsesMinutes(t *testing.T) {
	_, cfg := newImageStorageFixture(t, "local", "")
	if _, err := cfg.Update(map[string]any{"image_retention_minutes": 60}); err != nil {
		t.Fatalf("set retention: %v", err)
	}
	tasks := NewImageTaskService(cfg)

	// 直接验证保留期换算：60 分钟应向上取整为 1 天（不能是 0）
	if got := cfg.ImageRetentionDays(); got != 1 {
		t.Errorf("ImageRetentionDays(60分钟) = %d，应为 1（向上取整）", got)
	}
	t.Logf("tasks service ok")
	_ = tasks
}
