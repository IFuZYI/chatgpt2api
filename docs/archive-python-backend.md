# 已归档：Python 后端（1.x）

2.0 起后端已全量重写为 Go。本文说明原 Python 实现去哪了、为什么移除、以及怎么查阅。

## 归档位置

Python 实现**完整保存在分支 `archive/python-backend-2.0`**：

```bash
git fetch origin archive/python-backend-2.0
git checkout archive/python-backend-2.0
```

该分支是移除 Python 代码前的最后一个提交。整棵仓库当时有 497 个文件
（含 Go 后端与前端），其中 **Python 实现本身 99 个文件**：

| 路径 | 内容 |
|:--|:--|
| `api/` | FastAPI 路由层（78 条路由） |
| `services/` | 业务逻辑（号池、图片、备份、OAuth、sub2api…） |
| `utils/` | 工具（`sentinel.py` 的 PoW 实现、图片处理等） |
| `test/` | pytest 测试 |
| `scripts/` | 运维脚本 |
| `main.py` | 应用入口 |
| `pyproject.toml` / `uv.lock` | 依赖声明与锁文件 |
| `.python-version` | Python 版本（3.13） |

## 为什么从 main 移除

不是「清理历史包袱」这么简单，有三条具体理由：

1. **它会让人误判技术栈。** 仓库根目录同时存在 `main.py` 与 `cmd/server/`，
   新贡献者第一眼看到 Python 文件会以为后端还是 Python，去改那些文件然后发现不生效。
2. **它拖慢每一次构建。** `.dockerignore` 里原本要专门列一堆 Python 路径来排除
   （`api`、`services`、`utils`、`test`、`scripts`、`main.py`、`pyproject.toml`、`uv.lock`），
   少写一条就把无用代码带进构建上下文。
3. **它无法被验证。** Python 侧的测试早已不再运行（CI 只跑 `go test`），
   留着一套跑不起来的测试比没有测试更危险 —— 它会给人「有测试覆盖」的错觉。

## 为什么归档而不是直接删除

- **`utils/sentinel.py` 是 Go 移植的黄金基线。** `internal/upstream/sentinel_flow.go`
  的 fnv1a_32 实现、18 项 config 数组、`gAAAAAC`/`gAAAAAB` 前缀都逐字对齐它，
  测试里的 golden 值也是用它跑出来的。以后上游改协议、需要重新核对时，
  必须能读到这份原始实现。
- **行为对齐需要参照。** ADR-0003 的 golden 基线、ADR-0005 的三套错误信封、
  ADR-0006 的数据格式，都以 Python 版为准。有疑问时要能翻到原实现。
- **迁移期可能回滚。** 保留一个可运行的旧版本，比只有 Go 版更安全。

## 与 Go 版的对应关系

| Python | Go | 说明 |
|:--|:--|:--|
| `api/` | `internal/api/` | 78 条路由 → 77 条（删了 `/api/third-party-apps`） |
| `services/account_service.py` | `internal/service/account*.go` | 号池、刷新、重登 |
| `services/image_service.py` | `internal/service/image*.go` | 图片、索引、标签、清理 |
| `services/config.py` | `internal/config/` | 归一化、默认值 |
| `utils/sentinel.py` | `internal/upstream/sentinel_flow.go` | **fnv1a_32**，密码登录用 |
| （chat 的 PoW） | `internal/upstream/pow.go` | **SHA3-512**，与 sentinel 是两套算法 |
| `main.py` | `cmd/server/main.go` | 入口、依赖接线 |

> ⚠️ 两个 PoW 算法**不能混用**：`pow.go` 是 SHA3-512 + 25 项 config（chat requirements），
> `sentinel_flow.go` 是 fnv1a_32 + 18 项 config（password_verify）。
> 前缀都是 `gAAAAAC`/`gAAAAAB`，很容易看错。

## 后续如何彻底删除

如果确认不再需要参照（比如上游协议彻底变了、或已有新的基线），可以：

```bash
git branch -D archive/python-backend-2.0          # 删本地分支
git push origin --delete archive/python-backend-2.0  # 删远程分支
```

但**不建议**重写历史去抹掉 Python 代码：那会让所有 fork 与 PR 失效，
而 Python 代码本身不含凭据（`config.json` 早已从历史中清理）。
