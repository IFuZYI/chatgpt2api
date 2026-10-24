package service

import (
	"sync/atomic"
	"testing"
	"time"
)

// 备份调度器必须按 interval_minutes 触发。
//
// 回归点：Go 侧完全没有调度器 —— backup.interval_minutes 被保存进
// BackupSettings.IntervalMin 后再无任何读取点，用户开了「自动备份」
// 永远不会备份，R2 永远空。Python 有 backup_service.start()（app.py:26）
// 每 30 秒跑一次 run_scheduled_backup_if_needed。
func TestBackupSchedulerRunsWhenDue(t *testing.T) {
	var runs atomic.Int64
	scheduler := NewBackupScheduler(BackupSchedulerOptions{
		Interval:       30 * time.Millisecond,
		Enabled:        func() bool { return true },
		IntervalMin:    func() int { return 1 },
		LastFinishedAt: func() string { return "" }, // 从未备份过 → 立即到期
		Running:        func() bool { return false },
		Run:            func(trigger string) { runs.Add(1) },
	})

	scheduler.Start()
	defer scheduler.Stop()

	deadline := time.After(2 * time.Second)
	for runs.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("scheduler never triggered a backup")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// 未启用时不能备份。
func TestBackupSchedulerSkipsWhenDisabled(t *testing.T) {
	var runs atomic.Int64
	scheduler := NewBackupScheduler(BackupSchedulerOptions{
		Interval:       20 * time.Millisecond,
		Enabled:        func() bool { return false },
		IntervalMin:    func() int { return 1 },
		LastFinishedAt: func() string { return "" },
		Running:        func() bool { return false },
		Run:            func(trigger string) { runs.Add(1) },
	})

	scheduler.Start()
	defer scheduler.Stop()

	time.Sleep(200 * time.Millisecond)
	if runs.Load() != 0 {
		t.Errorf("disabled scheduler ran %d backups, want 0", runs.Load())
	}
}

// 距上次完成未满间隔时不能备份。
func TestBackupSchedulerRespectsInterval(t *testing.T) {
	var runs atomic.Int64
	scheduler := NewBackupScheduler(BackupSchedulerOptions{
		Interval:    20 * time.Millisecond,
		Enabled:     func() bool { return true },
		IntervalMin: func() int { return 360 }, // 6 小时
		LastFinishedAt: func() string {
			return time.Now().UTC().Format(time.RFC3339) // 刚备份完
		},
		Running: func() bool { return false },
		Run:     func(trigger string) { runs.Add(1) },
	})

	scheduler.Start()
	defer scheduler.Stop()

	time.Sleep(200 * time.Millisecond)
	if runs.Load() != 0 {
		t.Errorf("scheduler ran %d backups within the interval, want 0", runs.Load())
	}
}

// 已有备份在跑时不能重复触发。
func TestBackupSchedulerSkipsWhileRunning(t *testing.T) {
	var runs atomic.Int64
	scheduler := NewBackupScheduler(BackupSchedulerOptions{
		Interval:       20 * time.Millisecond,
		Enabled:        func() bool { return true },
		IntervalMin:    func() int { return 1 },
		LastFinishedAt: func() string { return "" },
		Running:        func() bool { return true }, // 永远在跑
		Run:            func(trigger string) { runs.Add(1) },
	})

	scheduler.Start()
	defer scheduler.Stop()

	time.Sleep(200 * time.Millisecond)
	if runs.Load() != 0 {
		t.Errorf("scheduler ran %d backups while one was running, want 0", runs.Load())
	}
}

// Stop 必须让循环退出（不能泄漏 goroutine）。
func TestBackupSchedulerStopExits(t *testing.T) {
	scheduler := NewBackupScheduler(BackupSchedulerOptions{
		Interval:       10 * time.Millisecond,
		Enabled:        func() bool { return false },
		IntervalMin:    func() int { return 1 },
		LastFinishedAt: func() string { return "" },
		Running:        func() bool { return false },
		Run:            func(trigger string) {},
	})

	scheduler.Start()
	done := make(chan struct{})
	go func() {
		scheduler.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within 2s — goroutine leak")
	}
}
