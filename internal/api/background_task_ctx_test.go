package api

import (
	"context"
	"testing"
	"time"
)

// 后台图片任务的 ctx 必须带兜底超时。
//
// 现状：runImageTask 用 context.Background()，而上游请求的超时完全由
// 调用方 ctx 决定（upstream.Client 的 TimeoutSec=0）。于是「上游 TCP 挂起
// 但不返回」时，任务 goroutine 永久不结束、**账号槽位永久被占**，
// ImageTaskService.Close 也只能等满超时后放弃。
//
// 这里不断言具体实现，只锁住不变量：后台任务拿到的 ctx 必须有 deadline。
func TestBackgroundTaskContextHasDeadline(t *testing.T) {
	ctx, cancel := backgroundTaskContext(context.Background())
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("后台任务 ctx 没有 deadline —— 上游挂起时任务与槽位会永久泄漏")
	}
}

// 已有 deadline 的父 ctx 不应被延长（取更早的那个）。
func TestBackgroundTaskContextRespectsEarlierDeadline(t *testing.T) {
	parent, pcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pcancel()
	ctx, cancel := backgroundTaskContext(parent)
	defer cancel()
	d, ok := ctx.Deadline()
	if !ok {
		t.Fatal("没有 deadline")
	}
	if d.After(time.Now().Add(3 * time.Second)) {
		t.Error("兜底超时延长了父 ctx 的 deadline")
	}
}
