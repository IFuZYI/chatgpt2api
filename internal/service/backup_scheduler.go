package service

import (
	"sync"
	"time"
)

// BackupScheduler 按 backup.interval_minutes 周期触发备份。
//
// 存在理由：配置里 `backup.enabled` 与 `backup.interval_minutes` 之前只被
// 保存和回显，没有任何组件读取它们 —— 用户开了「自动备份」永远不会备份，
// R2 永远是空的。Python 有对应的 backup_service.start()（api/app.py:26），
// 每 30 秒跑一次 run_scheduled_backup_if_needed（backup_service.py:326-351）。
//
// 语义逐条对齐 Python：
//   - 未启用 → 跳过
//   - 已有备份在跑 → 跳过（避免并发写同一个归档）
//   - 距上次完成未满 interval_minutes → 跳过
//   - 从未备份过（last_finished_at 为空）→ 立即备份
type BackupScheduler struct {
	options BackupSchedulerOptions

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// BackupSchedulerOptions 是调度器的依赖注入点。
//
// 全部用函数而不是具体类型：调度器只需要「问几个问题」和「做一件事」，
// 这样测试不需要构造完整的 BackupService + Config + Storage。
type BackupSchedulerOptions struct {
	// Interval 是检查周期（Python 是固定 30 秒）。
	Interval time.Duration
	// Enabled 报告 backup.enabled。
	Enabled func() bool
	// IntervalMin 报告 backup.interval_minutes（Python 默认 360）。
	IntervalMin func() int
	// LastFinishedAt 报告上次备份完成时间（RFC3339，空表示从未备份）。
	LastFinishedAt func() string
	// Running 报告是否已有备份在执行。
	Running func() bool
	// Run 触发一次备份，trigger 是触发来源（"schedule"）。
	Run func(trigger string)
}

// NewBackupScheduler 构造调度器（不启动）。
func NewBackupScheduler(options BackupSchedulerOptions) *BackupScheduler {
	if options.Interval <= 0 {
		options.Interval = 30 * time.Second
	}
	return &BackupScheduler{
		options: options,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start 启动后台循环。重复调用是空操作。
func (s *BackupScheduler) Start() {
	go s.loop()
}

// Stop 停止循环并等待 goroutine 退出。可重复调用。
func (s *BackupScheduler) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		<-s.done
	})
}

// loop 是调度主循环。
func (s *BackupScheduler) loop() {
	defer close(s.done)

	// 启动后先检查一次：进程重启后若已过期，不该再等一个周期。
	s.tick()

	ticker := time.NewTicker(s.options.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// tick 执行一次「该不该备份」的判断。
//
// panic 不能杀死循环：一次失败（比如状态文件损坏）不该让自动备份永久停摆。
func (s *BackupScheduler) tick() {
	defer func() {
		_ = recover()
	}()

	if s.options.Enabled == nil || !s.options.Enabled() {
		return
	}
	if s.options.Running != nil && s.options.Running() {
		return
	}
	if !s.due() {
		return
	}
	if s.options.Run != nil {
		s.options.Run("schedule")
	}
}

// due 判断距上次完成是否已满 interval_minutes。
//
// 解析失败按「从未备份」处理（立即备份）—— 与 Python 的
// `except Exception: pass` 后落到 run_backup 等价。
func (s *BackupScheduler) due() bool {
	lastFinished := ""
	if s.options.LastFinishedAt != nil {
		lastFinished = s.options.LastFinishedAt()
	}
	if lastFinished == "" {
		return true
	}

	parsed, err := time.Parse(time.RFC3339, lastFinished)
	if err != nil {
		return true
	}

	intervalMinutes := 360
	if s.options.IntervalMin != nil {
		if value := s.options.IntervalMin(); value > 0 {
			intervalMinutes = value
		}
	}
	return time.Since(parsed) >= time.Duration(intervalMinutes)*time.Minute
}
