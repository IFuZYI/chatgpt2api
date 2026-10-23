package api

import (
	"context"
	"time"
)

// contextBackground 给后台任务用（脱离请求 ctx）。
func contextBackground() context.Context { return context.Background() }

// nowISO 返回 RFC3339 时间串。
func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }
