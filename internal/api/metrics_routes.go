package api

import (
	"net/http"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
)

// registerMetricsRoutes 暴露运行期自检指标，供长稳观察与排障。
//
// 为什么需要：本服务是**单实例长驻进程**（ADR-0004），而此前没有任何
// 运行期指标 —— goroutine 泄漏、fd 泄漏、堆增长只能靠外部 ps 猜，
// 出问题时也没有堆栈可看。压测与长稳验证都缺一个观测点。
//
// 安全边界：**必须 admin 鉴权**。指标里含 goroutine 数量、堆大小、
// 启动时长 —— 对攻击者是有用的侦察信息（判断是否被刷、何时重启过），
// 不能像 /health 那样公开。
//
// 刻意**不**挂 net/http/pprof：它默认注册在 DefaultServeMux 上且
// 部分端点（/debug/pprof/profile）会让进程 CPU 跑满，暴露给管理面
// 需要额外的限流与超时设计，超出本次范围。这里只给计数型指标。
func (d *Deps) registerMetricsRoutes(r *gin.Engine) {
	r.GET("/api/metrics", func(c *gin.Context) {
		if _, err := d.RequireAdmin(c.GetHeader("Authorization")); err != nil {
			writeErr(c, err)
			return
		}

		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)

		c.JSON(http.StatusOK, gin.H{
			"runtime": gin.H{
				"goroutines":  runtime.NumGoroutine(),
				"heap_bytes":  mem.HeapAlloc,
				"heap_sys":    mem.HeapSys,
				"stack_bytes": mem.StackInuse,
				"num_gc":      mem.NumGC,
				"gc_pause_ns": mem.PauseTotalNs,
				"uptime_secs": int64(time.Since(d.startedAt).Seconds()),
				"go_version":  runtime.Version(),
				"num_cpu":     runtime.NumCPU(),
			},
			"accounts": d.Accts.Stats(),
		})
	})
}
