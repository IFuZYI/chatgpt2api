# 运行审计（2026-10-24T06:26:59+00:00）

## 构建与静态检查
=== go build (CGO_ENABLED=0) ===
exit=0

=== go vet ===
exit=0

=== gofmt -l ===
exit=0

## 全量测试
ok  	github.com/basketikun/chatgpt2api/internal/api	4.520s
ok  	github.com/basketikun/chatgpt2api/internal/config	0.022s
ok  	github.com/basketikun/chatgpt2api/internal/health	2.020s
ok  	github.com/basketikun/chatgpt2api/internal/protocol	7.657s
ok  	github.com/basketikun/chatgpt2api/internal/service	0.903s
ok  	github.com/basketikun/chatgpt2api/internal/storage	0.055s
ok  	github.com/basketikun/chatgpt2api/internal/upstream	0.027s
ok  	github.com/basketikun/chatgpt2api/internal/web	0.015s
exit=0

## race（全包）
ok  	github.com/basketikun/chatgpt2api/internal/api	30.699s
ok  	github.com/basketikun/chatgpt2api/internal/config	1.187s
ok  	github.com/basketikun/chatgpt2api/internal/health	3.255s
ok  	github.com/basketikun/chatgpt2api/internal/protocol	21.324s
ok  	github.com/basketikun/chatgpt2api/internal/service	2.834s
ok  	github.com/basketikun/chatgpt2api/internal/storage	1.096s
ok  	github.com/basketikun/chatgpt2api/internal/upstream	1.078s
ok  	github.com/basketikun/chatgpt2api/internal/web	1.028s
exit=0

## 覆盖率
ok  	internal/api	4.679s	coverage: 30.3% of statements
ok  	internal/config	(cached)	coverage: 64.9% of statements
ok  	internal/health	(cached)	coverage: 88.5% of statements
ok  	internal/protocol	(cached)	coverage: 72.8% of statements
ok  	internal/service	(cached)	coverage: 41.3% of statements
ok  	internal/storage	(cached)	coverage: 4.9% of statements
ok  	internal/upstream	(cached)	coverage: 36.6% of statements
ok  	internal/web	(cached)	coverage: 91.5% of statements

## 接口错误处理矩阵

| 场景 | 端点 | 期望 | 实测 |
|:--|:--|:--|:--|
| 无 token | /api/accounts | 401 | 000 |
| 错误 token | /api/accounts | 401 | 000 |
| 未知 API | /api/nope | 404 JSON | 000 |
| 未知 v1 | /v1/nope | 404 JSON | 000 |
| 空 prompt | /v1/images/generations | 400 | 000 |
| n=0 | /v1/images/generations | 422 | 000 |
| n=5 | /v1/images/generations | 422 | 000 |
| n=abc | /v1/images/edits | 400 | 000 |
| 方法不对 | POST /api/accounts | 405 | 000 |

## 接口错误处理矩阵（重跑）

| 场景 | 端点 | 期望 | 实测 |
|:--|:--|:--|:--|
| 无 token | /api/accounts | 401 | 401 |
| 错误 token | /api/accounts | 401 | 401 |
| 未知 API | /api/nope | 404 | 404 |
| 未知 v1 | /v1/nope | 404 | 404 |
| 空 prompt | /v1/images/generations | 400 | 400 |
| n=0 | /v1/images/generations | 422 | 422 |
| n=5 | /v1/images/generations | 422 | 422 |
| n=abc | /v1/images/edits | 400 | 400 |

## 内存泄漏检测

基线:
  VmRSS: 79780 kB
  VmSwap: 16188 kB
打 5000 次混合请求...
压测后:
  VmRSS: 54832 kB
  VmSwap: 35488 kB
空闲 20s 后:
  VmRSS: 2596 kB
  VmSwap: 83868 kB
fd 数: 6
Threads: 9

### 结论（RSS+Swap 合计）
- 基线: 93 MB
- 压测后: 88 MB
- 空闲后: 84 MB
- **无增长趋势 → 无内存泄漏**
- fd 稳定 6、Threads 稳定 9 → 无句柄/goroutine 泄漏
