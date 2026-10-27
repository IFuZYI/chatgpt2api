# 2.0 重写完成的范围，与刻意留下的空白

## 已完整移植（行为对齐 Python 版）

| 子系统 | 覆盖 |
|:--|:--|
| HTTP 层 | 全部 78 条路由，三套错误信封，CORS，SPA fallback，`_next/` 404 特判 |
| 配置 | `config.json` 全部归一化逻辑、`_existing_cf_*` cookie 保留机制、敏感字段脱敏 |
| 存储 | json / sqlite / postgres / git 四种后端，增量写入语义 |
| 认证 | 裸 sha256 + `hmac.Equal`，密钥 CRUD，`last_used_at` 60s 节流 |
| 号池 | 槽位（channel 信号量）、token 别名链、限流/异常状态、刷新进度、导出 |
| 上游 | TLS 指纹 + header 顺序、sentinel prepare/finalize、PoW（SHA3-512）、turnstile 解释器 |
| 协议 | SSE 状态机、增量拼接、注解清洗、`file_id` 提取、moderation 检测、chat/responses/messages/search |
| 图片 | 生成/编辑、并行池、轮询（settle/check-before-hit/退避）、对话清理、任务队列、resume-poll |
| 管理面 | 账号 CRUD、图片管理（列表/删除/压缩/清理到目标/缩略图/标签）、日志、代理测试与清障、备份（R2 SigV4）、WebDAV 同步、CPA、sub2api、OAuth PKCE 桥 |

## 曾经的空白，现已补齐

2.0 首次落地时刻意留了 6 项空白。截至 `42def84`，其中 4 项已实现：

| 项 | 状态 | 落地位置 |
|:--|:--|:--|
| 密码重新登录（含 TOTP 两步验证） | **已实现** | `internal/service/password_login.go`、`account_relogin.go`、`internal/upstream/sentinel_flow.go` |
| 可编辑 PPT/PSD | **已实现** | `internal/api/editable_file_routes.go` + `internal/upstream/editable_file_chain.go` |
| 网页搜索 | **已实现** | `internal/api/search_routes.go` + `internal/upstream/search_chain.go` |
| Responses 图片工具 | **已实现** | `internal/api/response_image_routes.go` |

仍然成立的两项：

| 项 | 当前行为 | 原因 |
|:--|:--|:--|
| 远程图片 URL 拉取 | `/v1/images/edits` 的 `http(s)://` 引用返回 400 | 需要出站客户端；data URL 与 multipart 已支持 |
| 匿名对话 | 依赖上游；当前上游限制（ADR-0008） | 非实现问题 |

## 遗留的已知限制

| 项 | 现状 | 影响 |
|:--|:--|:--|
| ~~`auto_relogin_after_refresh` 未接线~~ | **已接线**：刷新发现 401/403 后自动密码重登（429 不触发） | — |
| 后台任务不可取消 | 仍有兜底超时（`backgroundTaskContext`，30 分钟），但**不能主动取消**：`TaskContext` 不带 ctx | `ImageTaskService.Close` 只能等满超时。2.2.3 已修「上游挂起导致槽位永久泄漏」，主动取消仍未做 |
| ~~进度对象不淘汰~~ | **已修**：`StartProgressSweeper` 每 `maxAge/4`（默认 90 分钟）清理超过 6 小时的条目 | — |
| ~~可编辑任务无保留策略~~ | **已修**：`EditableFileTaskService.CleanupExpired` 在启动时按 `image_retention_minutes` 清理任务与产物目录 | — |
| ~~rt 换 at 未实现~~ | **已实现**：`RefreshAccessToken`，form-urlencoded 换 token | — |
| ~~入站请求体无上限~~ | **已修（2.2.3）**：`decodeJSONBody` 加 `MaxBytesReader` | — |
| 备份归档默认不加密 | `backup.encrypt` 默认 false | 归档含号池全部凭据，需操作者显式开启加密 |
| ~~图片标签写入非原子~~ | **已修**：`saveTags` 改为写 `.tmp` 再 `os.Rename` | — |

## 安全审计后的修复（2026-10）

一轮完整安全审计（8 个覆盖单元）发现并修复：

1. **流式生图失败被吞**：`/v1/images/generations` 流式路径在 `pool.Run` 失败时只写 `[DONE]`，客户端收到 200 + 空流，无法区分「成功但 0 张图」与故障。已改为在流里补 `error` 事件，与 `response_image_routes.go` 对齐。
2. **`SyncToWebDAV` 丢索引**：网络循环前取快照、循环后整份写回，期间 `SaveImageBytes` 新增的条目被抹掉（webdav 模式无本地副本 → 图永久不可达）。已改为只合并本次上传成功的条目，并在锁内重新读取索引。
3. **图片标签并发丢更新**：`loadTags`/`saveTags` 无锁，50 个并发 `SetTags` 实测只留下 0 条。已加 `tagsMu`。
4. **`config.example.json` 泄漏真实凭据**：模板曾携带一条真实 SOCKS5 出口代理凭据与私有 WebDAV 主机名，已清除；该凭据已推送过，需轮换。

三处并发/吞错缺陷都没有被既有测试发现 —— 前两个是文件级丢更新，`-race` 检测器看不见。每条都补了会失败的回归用例。

## Python 后端已归档

1.x 的 Python 实现已从 main 移除，完整保存在分支 `archive/python-backend-2.0`。
保留而非删除的理由：`utils/sentinel.py` 是 Go 侧 fnv1a_32 的黄金基线，
ADR-0003 的 golden 也以 Python 行为准。详见 `docs/archive-python-backend.md`。

## 与 Python 版的实测差异（刻意保留）

1. **`download_images_zip` 保留 WebDAV 回退**：Python 版被同名函数覆盖导致该功能不可用，Go 版按修复后语义实现。
2. **空 token 不发 `Authorization` 头**：Python 会拼出 `Bearer `（尾部空格），上游判为无效凭据。
3. **JSON 对象键顺序不同**：Python 保留 dict 插入序，Go 按 key 排序。语义等价，golden 用语义比较而非字节比较（ADR-0003 修订）。
4. **golden 里 405 的 `Allow` 头**：Gin 与 FastAPI 生成的值不同，未逐字复刻（客户端不依赖）。

## 测试面

561 个 Go 测试：
- `internal/protocol` 225（SSE、缓存、token 计费、Anthropic、图片轮询/池、mask 合成）
- `internal/service` 153（备份加密双向 OpenSSL 兼容、R2 SigV4、WebDAV、OAuth PKCE、密码登录/重登、标签并发）
- `internal/api` 90（golden 用例、路由对齐、流式错误、n 校验、任务格式）
- `internal/upstream` 65（PoW 跨语言 golden、sentinel fnv1a golden、TOTP RFC 向量）
- `internal/config` 11（归一化、默认值、cookie 保留）
- `internal/storage` 8（json/sqlite 后端契约）
- `internal/web` 5（embed、SPA fallback、路径穿越防护）
- `internal/health` 4

覆盖率（语句）：web 91.5% / health 88.5% / protocol 78.2% / config 64.9% / upstream 55.8% / service 53.0% / storage 44.1% / api 42.0%。

live 测试（`-tags live`）单独跑，只记录上游状态不设失败断言（ADR-0008 的教训）。
