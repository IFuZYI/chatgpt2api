package service

import (
	"fmt"
	"sync"
	"time"
)

// ImageCleanupScheduler 复刻 Python services/image_service.py 的
// _auto_cleanup_worker：每 30 分钟检查磁盘，低于阈值时清理最旧图片。
//
// 为什么必须有它：长期运行的实例会持续写图，磁盘写满后所有生图都会失败，
// 而且失败原因（ENOSPC）对用户毫无提示。Python 靠这个循环兜住。
type ImageCleanupScheduler struct {
	actions CleanupActions
	cfg     CleanupConfig

	stop chan struct{}
	once sync.Once
	done chan struct{}
}

// CleanupActions 是调度器要执行的三件事。
//
// 用函数而非接口：这三个动作分散在 config（按保留期清理）和
// ImageService（磁盘统计、按目标清理）两处，硬套一个接口会让
// 两个不相关的类型都被迫实现对方的方法。
type CleanupActions struct {
	CleanupOldImages func() int
	DiskFreeMB       func() int
	DeleteToTarget   func(targetMB int, dryRun bool) map[string]any
}

// CleanupConfig 是调度器需要的配置访问器。
type CleanupConfig interface {
	ImageMinFreeMB() int
}

// 与 Python 一致的检查间隔。
const imageCleanupInterval = 30 * time.Minute

// NewImageCleanupScheduler 构造。
func NewImageCleanupScheduler(actions CleanupActions, cfg CleanupConfig) *ImageCleanupScheduler {
	return &ImageCleanupScheduler{
		actions: actions,
		cfg:     cfg,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start 启动后台循环。
func (s *ImageCleanupScheduler) Start() {
	go s.run()
}

// Stop 停止循环并等待退出。
func (s *ImageCleanupScheduler) Stop() {
	s.once.Do(func() { close(s.stop) })
	<-s.done
}

func (s *ImageCleanupScheduler) run() {
	defer close(s.done)

	// Python 先 wait(1800) 再执行，所以启动后不立即清理
	//（启动时 main 已经跑过一次 CleanupOldImages）。
	ticker := time.NewTicker(imageCleanupInterval)
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

// tick 执行一轮检查 + 清理。
func (s *ImageCleanupScheduler) tick() {
	defer func() {
		// 单轮失败不能杀死循环（Python 也是 try/except 吞掉），
		// 但**必须留下痕迹**：静默吞掉会让「清理再也不跑」变成
		// 一个无人知晓的故障（磁盘慢慢被图片填满）。
		if recovered := recover(); recovered != nil {
			fmt.Printf("[image-cleanup] tick panicked, recovered: %v\n", recovered)
		}
	}()

	if s.actions.CleanupOldImages != nil {
		s.actions.CleanupOldImages()
	}

	threshold := s.cfg.ImageMinFreeMB()
	if threshold < 1 {
		threshold = 500
	}
	if s.actions.DiskFreeMB == nil || s.actions.DeleteToTarget == nil {
		return
	}
	if free := s.actions.DiskFreeMB(); free < threshold {
		s.actions.DeleteToTarget(threshold, false)
	}
}
