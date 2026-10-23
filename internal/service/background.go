package service

import (
	"context"
	"sync"
	"time"
)

// BackgroundWork 登记「会写 data/ 的可取消后台工作」：账号刷新、CPA / sub2api
// 导入、resume-poll 等。
//
// 为什么需要它：这些工作跑在自己的 goroutine 里，生命周期远长于一次请求
// （账号刷新用 30 分钟 ctx）。进程退出时如果不等它们，JSON 后端可能正写到一半；
// 但如果只是「等」，30 分钟的刷新会让关闭永远超时。所以语义是
// **先取消、再等待**：关闭时取消它们派生的 ctx，让它们尽快收尾，再等它们退出。
//
// 与 ImageTaskService 本身的在途任务不同：图片生成任务不可取消
// （生图打一半被砍会在上游留下孤儿对话，见 ADR-0007），只能等或超时放弃。
type BackgroundWork struct {
	mu      sync.Mutex
	closing bool
	nextID  uint64
	cancels map[uint64]context.CancelFunc
	wg      sync.WaitGroup
}

// NewBackgroundWork 构造一个空注册表。
func NewBackgroundWork() *BackgroundWork {
	return &BackgroundWork{cancels: map[uint64]context.CancelFunc{}}
}

// Start 派生一个受管的 ctx，并登记到注册表。
//
// 返回的 ctx 会在两种情况下被取消：调用方显式调用 finish，或服务关闭。
// 调用方必须调用 finish（defer 即可），否则注册表会泄漏一个 cancel。
//
// 服务已进入关闭流程时返回 ok=false —— 调用方应放弃执行，而不是起一个
// 关闭过程中不会被等待的 goroutine。
func (b *BackgroundWork) Start(parent context.Context, timeout time.Duration) (context.Context, func(), bool) {
	if parent == nil {
		parent = context.Background()
	}

	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	id := b.nextID
	b.nextID++
	b.cancels[id] = cancel
	b.wg.Add(1)
	b.mu.Unlock()

	var once sync.Once
	finish := func() {
		once.Do(func() {
			cancel()
			b.mu.Lock()
			delete(b.cancels, id)
			b.mu.Unlock()
			b.wg.Done()
		})
	}
	return ctx, finish, true
}

// shutdown 停止接受新的工作，并取消所有在途工作的 ctx。
func (b *BackgroundWork) shutdown() {
	b.mu.Lock()
	b.closing = true
	cancels := make([]context.CancelFunc, 0, len(b.cancels))
	for _, cancel := range b.cancels {
		cancels = append(cancels, cancel)
	}
	b.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}

// wait 等待所有已登记的工作退出（不含 ctx 超时控制，由调用方的 select 负责）。
func (b *BackgroundWork) wait() { b.wg.Wait() }

// pending 返回在途工作数（用于关闭超时日志）。
func (b *BackgroundWork) pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.cancels)
}
