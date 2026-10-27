# 性能特征与基准

本文件记录关键热路径的实测数据与优化依据，供后续改动做回归对比。
数字来自 2 核 / Intel Xeon Gold 6133 的容器环境，**绝对值不可移植，
比值与分配次数才是重点**。

## 如何复现

```bash
# 串行基准
go test ./internal/service/ -bench='BenchmarkGetTextAccessToken|BenchmarkAcquireImageSlot|BenchmarkProxyFor|BenchmarkListMaintainableTokens' \
  -benchtime=2000x -benchmem -run=^$

# 并行基准（验证锁竞争）
go test ./internal/service/ -bench='Parallel' -benchtime=3000x -benchmem -run=^$
```

`internal/service/hotpath_bench_test.go` 与 `conc_bench_test.go` 是基准源码。

## 热路径数据（300 账号）

| 路径 | 触发频率 | 串行 | 并行 | 分配 |
|:--|:--|:--|:--|:--|
| `GetTextAccessToken` | 每个 `/v1/chat/completions` | 39.9 µs | 33.9 µs | **0 allocs** |
| `AcquireImageSlot` | 每个图片请求 | 45.3 µs | 55.9 µs | 4 allocs / 9.8 KB |
| `ProxyFor` | 每个上游请求 | 307 ns | 197 ns | 0 allocs |
| `ListMaintainableTokens` | watcher 每轮 | 89 µs | — | 1 allocs |

## 已做的优化与依据

### 1. 文本选号：池化候选切片（2.2.3）

原实现每次 `make([]string, 0, len(accounts))`。300 账号时每次请求分配
~5.4 KB，高 QPS 下是 GC 的主要压力源。改用 `sync.Pool` 借还后：

```
优化前 43825 ns/op   5400 B/op   1 allocs/op
优化后 39911 ns/op      2 B/op   0 allocs/op
```

**归还前必须 `clear` 底层数组**：只写 `s[:0]` 长度归零但底层数组仍持有
token 字符串引用，GC 无法回收 —— 等于用省分配换来一处常驻泄漏。
`TestTextCandidateBufferClearedOnPut` 守住这条。

### 2. 图片轮转：去掉重复 slice 分配（2.2.3）

`append(append([]string{}, ready[start:]...), ready[:start]...)` 内层分配
一次、外层扩容一次，共两个 slice。改为一次 `make` + 两次 `copy`：

```
优化前 67169 ns/op  12508 B/op  5 allocs/op
优化后 60472 ns/op   9819 B/op  4 allocs/op
```

### 3. 图片取号：写锁改读锁（2.2.3）

`acquireSlot` 用 `s.mu.Lock()` 保护一个**只读**遍历，把并发取号串行化了。
实测并行比串行还慢（80.3 µs vs 60.5 µs）——写锁争用的典型表现。
改 `RLock` 后并行 55.9 µs（-30%）。安全性：`cursor` 是 `atomic.Uint64`。

## 未做的优化及原因

- **`readyCandidateTokensLocked` 没有池化**：它在锁内被调用，把缓冲的
  借还跨进持锁区间会让生命周期难以推理，收益不值这个复杂度。
- **没有为 `ListMaintainableTokens` 做增量索引**：它只在 watcher 每轮
  调用一次（默认 5 分钟），89 µs 完全可以接受。维护索引的一致性成本
  远高于收益。

## 观察到的上限

- 文本选号是 O(n) 遍历全池。300 账号 40 µs 意味着单核约 25k QPS 的
  选号能力 —— 远高于上游账号池的实际承载，暂不需要索引化。
- 真正瓶颈在上游（单账号图片生成可达数十秒），不在本地选号。
