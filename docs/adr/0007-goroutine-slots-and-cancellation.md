# 图片生成用 goroutine + 每账号信号量，清理路径脱离请求 ctx

Python 版的并发模型是：每个 HTTP 请求占一个线程，`n` 张图再开 `n` 个线程，每张图在 `_image_slot_condition` 上 `wait(timeout=1.0)` 轮询等槽位。客户端断连后线程继续跑完整个上游轮询，最后才走 `_remove_image_conversation_later` 清上游对话。

Go 版直接照搬 `ctx` 取消会引入一个 Python 版没有的 bug：客户端断连 → ctx 取消 → 提前 return → **跳过上游对话清理** → 上游堆积孤儿对话，而清理恰恰是那个"用户已经走了更需要做"的动作。

决定：请求 ctx 只控制「是否继续往客户端写」；上游轮询与对话清理跑在 `context.WithoutCancel(ctx)` 派生的 ctx 上，带自己的硬超时（沿用 `image_poll_timeout_secs`）。账号槽位从 `Condition + 1s 轮询` 改成每账号带缓冲 channel（容量 = `image_account_concurrency`），获取用 `select` 同时监听 `ctx.Done()`，释放走 `defer`。等槽位的语义保留 Python 的两种结局：**没有任何就绪账号** → 立即报 `no available image quota`；**有就绪账号但都满** → 阻塞等待，不报错。

## Consequences

- 槽位释放必须 `defer`，且必须在 `mark_image_result` 之前完成——Python 版 `mark_image_result` 内部自己调 `release_image_slot`，这个顺序在 Go 里要显式化，否则双重释放或泄漏。
- `image_inflight` 变成 `atomic.Int64` 的映射，`/api/accounts` 读它做展示，允许轻微过期。
- 客户端断连后仍会占用槽位直到上游返回，这与 Python 版行为一致，是刻意保留的，不是 bug。
- 硬超时看门狗（`_iter_sse_payloads_capped` 的 `threading.Timer(response.close)`）在 Go 里变成 `time.AfterFunc` + `resp.Body.Close()`，用于打断阻塞的 SSE 读取。
