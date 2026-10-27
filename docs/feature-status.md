# 功能状态

本文基于当前仓库当前实现整理，用于帮助用户快速了解哪些功能已经可用、哪些仍在完善、哪些待实现。

| 功能 | 状态 | 说明 |
|:----------------------------------------|:--:|:--------------------------------------------------------------|
| OpenAI 兼容 `POST /v1/images/generations` | ✅  | 已支持，用于图片生成，并可通过 `n` 返回多张图片。 |
| OpenAI 兼容 `POST /v1/images/edits` | ✅  | 已支持，可上传图片进行编辑。 |
| 面向图片工作流的 `POST /v1/chat/completions` | ✅ | 图片请求（图片模型或 `modalities` 含 `image`）走生图链路，结果转成 markdown 图片 + annotations；网页搜索请求（`web_search_options` 或 search 模型前缀）走搜索链路并附 `url_citation`。 |
| 面向图片工作流的 `POST /v1/responses` | ✅ | 请求含 `tools:[{"type":"image_generation"}]` 时走生图链路，输出 Responses 格式事件。 |
| `GET /v1/models` 接口 | ✅  | 动态聚合：文本模型来自上游 `/backend-api/models`（当前为 `auto`、`gpt-5-6`、`gpt-5-6-mini`、`gpt-6`、`gpt-6-mini`、`research` 等），图片模型为内置表（`gpt-image-2`、`gpt-image-2.5`、`gpt-image-2.5-flare`、`gpt-image-2.5-sunburst`）。具体列表随号池变化。 |
| 同时生成多张图片 | ✅  | 已支持，后端与前端都可进行多图生成。 |
| 图片并行生成 | ✅  | 多张图片使用独立线程和账号同时生成，可通过 `image_parallel_generation` 配置关闭。 |
| 图片生成进度追踪 | ✅  | 任务显示当前步骤（上传/预热/获取token/生成中等），支持耗时统计。 |
| 图片超时续轮询 | ✅  | 超时任务可继续等待，前端显示"继续等待"按钮，后端 resume-poll API。 |
| 图片二次确认与先check再hit | ✅  | 可通过 `image_settle_enabled` 和 `image_check_before_hit_enabled` 配置，关闭后跳过等待直接返回。 |
| 前端图片工作台 | ✅  | 已支持图片生成、图片编辑、模型选择、历史记录与查看大图。 |
| 前端图片懒加载与滚动优化 | ✅  | LazyImage 懒加载、会话切换滚动位置保存与恢复、bfcache 页面恢复同步。 |
| 前端图片输入 / 参考图交互 | ✅  | 已支持参考图上传、预览、移除和编辑模式工作流。 |
| Codex 画图接口逆向 | ⚠️  | 走 `/backend-api/codex/responses`，仅 `Plus` / `Team` / `Pro` 且 `source_type=codex` 的账号可用，模型别名为 `codex-gpt-image-2`。请求侧与 Python 逐字对齐；**响应事件格式未经真实上游验证**，若上游改用分片事件会报 `No image result found in response`。详见 `docs/adr/0010-codex-responses-response-shape-unverified.md`。 |
| Cherry Studio 接入 | ✅  | 已支持作为绘图接口接入 Cherry Studio。 |
| New API 接入 | ✅  | 已支持接入 New API。 |
| 账号池管理 | ✅  | 已支持列表、筛选、批量操作、导出、手动编辑、刷新和删除。 |
| 账号刷新异步进度追踪 | ✅  | 刷新和重新登录改为异步模式，前端轮询显示进度。 |
| 密码重新登录恢复异常账号 | ✅  | 号池管理页面支持重新登录（含 TOTP 两步验证）。`auto_relogin_after_refresh` **已接线**：刷新发现失效账号（401/403）时自动重登；429 限流不触发（避免上游抖动导致批量登录）。 |
| 账号额度刷新与恢复时间同步 | ✅  | 已支持账号信息刷新，限流账号也会自动继续检查。 |
| 失效 Token 自动清理 | ✅  | 已支持自动移除失效 Token。 |
| CPA 连接管理 | ✅  | 已支持 CPA 连接的新增、修改、查询和删除。 |
| CPA 文件浏览与按需导入 | ✅  | 已支持读取远程文件列表、筛选、勾选并导入到本地号池。 |
| CPA 导入进度跟踪 | ✅  | 已支持导入进度展示与轮询更新。 |
| `sub2api` 连接管理与账号浏览 | ✅  | 已支持 `sub2api` 服务器的新增、修改、删除、分组查询和 OpenAI OAuth 账号列表读取。 |
| `sub2api` 导入 | ✅  | 已支持勾选 `sub2api` 中的 OpenAI OAuth 账号，批量拉取 `access_token` 导入本地号池，并展示导入进度。 |
| Docker 自托管部署 | ✅  | 三套 compose：默认（拉 GHCR 镜像）、`local`（本地构建 + sqlite）、`warp`（WARP + FlareSolverr 清障）。`data/` 属主由内置 `init-data` 自动修正，`warp` 的配置初始化由 `--init-proxy-config` 子命令完成。多架构镜像（amd64/arm64）。 |
| 兼容接口中的多参考图能力 | ✅  | 已实现，支持在兼容接口中传入多参考图。 |
| 更高级的 Token 调度策略 | ⚠️ | 当前已有轮询、限流刷新、槽位并发控制；更复杂的调度策略仍在完善中。 |
| Render / Vercel 等部署表述 | ⚠️ | 当前主要以 Docker 部署为主，其他平台部署方式暂未重点说明。 |
| `/v1/complete` 文本补全 | ❌  | 该端点从未存在，文档早期误记。文本能力走 `/v1/chat/completions`。 |
| 流式输出支持 | ⚠️  | 协议层已实现（SSE 分块、`data: [DONE]` 结尾、chunk 形状与 Python 一致），但**生成期间不产生增量**：图片与文本都是先跑完再一次性写出所有 chunk。长请求期间客户端收不到字节，nginx/Cloudflare 的 60s 空闲会切断连接。 |
| 内容过滤（敏感词 + AI 审核） | ✅  | `sensitive_words` 命中直接 400；`ai_review.enabled` 时调 OpenAI 兼容审核服务，按 `fail_open`（默认 true）决定故障时放行还是 503。接入 5 个入口：`/v1/images/generations`、`/v1/images/edits`、两个任务接口、文本对话。检查在取号/入队之前，不消耗额度。 |
| 文本补全缓存与重复请求合并 | ✅  | `/v1/chat/completions` 文本链路默认启用 60 秒短缓存、流式结果回放、in-flight 请求合并和相邻重复消息清理；可通过 `chat_completion_cache` 配置关闭或调整。 |
| 图片尺寸参数 | ✅  | `size` 与 `quality` 在 `/v1/images/generations`、`/v1/images/edits` 与任务接口均透传给上游。 |
| 服务端图片 URL 缓存 | ✅  | 已实现。 |
| `rt_token` 刷新 | ✅  | 通过 `refresh_token` 刷新 access_token，含 keepalive 周期。 |
| 代理配置功能 | ✅  | 已支持网页端配置全局 HTTP / HTTPS / SOCKS5 / SOCKS5H 代理，并应用到出站请求。 |
| Anthropic 协议支持 | ✅  | 已实现 `POST /v1/messages`，含 XML 工具调用适配。 |

## 2.0 新增说明

后端已从 Python/FastAPI 全量重写为 Go/Gin（见 `docs/adr/`）。上表功能在 Go 版中的实现状态：

| 项 | 状态 | 说明 |
|:--|:--|:--|
| 全部 80 条 HTTP 路由 | ✅ | 与 Python 版逐条对齐（Python 78 条；删除 `/api/third-party-apps`，后续新增 `/api/metrics` 等），`TestRouteParity` 与 `TestRouteCountMatchesDocumentedNumber` 常驻回归。 |
| 三套错误信封 | ✅ | 管理面 / OpenAI / Anthropic，28 个 golden 用例覆盖。 |
| 上游 TLS 指纹 | ✅ | `tls-client` + header 顺序，真实上游 sentinel 握手验证通过。 |
| 图片并行生成与槽位 | ✅ | goroutine + channel 信号量，槽位泄漏有测试。 |
| 备份加密 | ✅ | 原生 AES + PBKDF2，与 `openssl enc` 双向兼容（有测试）。 |
| 密码重新登录（含 TOTP） | ✅ | 走 authorize → sentinel（`fnv1a_32`，与 chat 的 SHA3-512 是不同算法）→ `password/verify` → TOTP → `mfa/verify`（参数是 `id` 不是 `factor_id`，且不带 sentinel）→ OAuth token 交换 → 回写 access/refresh/id。真实上游验证通过。 |
| 可编辑 PPT/PSD | ✅ | `POST /v1/ppt|psd/generations` 走上传参考图 → `prepare`（`gpt-5-5-thinking`/`extended`）→ `/f/conversation` → 轮询 `/backend-api/conversation/{id}` 拿 pptx/psd + zip → 落 `data/files/{kind}/{task_id}/`，`GET /files/*` 提供下载（防目录穿越）。任务队列与图片链路同构（queued/running/success/error、`Close()` 等在途、重启恢复）。 |
| 网页搜索 | ✅ | `POST /v1/search` 走 `prepare`（`system_hints:["search"]`）→ `/f/conversation` → 轮询 `/backend-api/conversation/{id}`，返回 answer + sources。超时 300s、间隔 3s、answer 连续 3 次一致即返回。 |
| Responses 图片工具 | ✅ | `/v1/responses` 带 `image_generation` 工具时已接入生图链路；失败时在流内补 `error` 事件再 `[DONE]`。 |
| 远程图片 URL 拉取 | ⚠️ | 返回 400（data URL 与 multipart 可用）。 |
| 匿名对话链路 | ❌ | 上游限制，非实现问题（见 `docs/adr/0008`）。 |

## 图片模型清单（2026-10 实测）

上游 `/backend-api/models` **不返回**图片模型（该端点只暴露文本模型），
所以图片模型无法自动发现，只能实测确认后写死在代码里
（`internal/protocol/models.go` 的 `baseImageModels`）。

| 模型 | 状态 | 备注 |
|:--|:--|:--|
| `gpt-image-2` | ✅ | 通用入口，上游模型由 `default_upstream_model_name` 决定（默认 `auto`） |
| `gpt-image-2.5` | ✅ | 模型名直接作为上游 slug 透传 |
| `gpt-image-2.5-flare` | ✅ | 同上 |
| `gpt-image-2.5-sunburst` | ✅ | 同上 |
| `codex-gpt-image-2` | ⚠️ | Codex 链路专用，带套餐前缀别名；响应格式待真实上游验证（ADR-0010） |

2.5 系列三个变体在同 prompt 下产出字节数各不相同（839KB / 853KB / 943KB，
均 1254×1254），确认是不同模型而非别名。

**新增模型时必须实测出图再收录**：只把名字加进列表不会让它工作，
还要确认 `_image_model_settings` 的映射分支是否覆盖（`gpt-image-2` 走配置，
其余走透传）。

## 模型目录的来源（重要）

`/v1/models` 的模型来自**两条完全不同的路径**，改代码时必须分开看：

| 类型 | 来源 | 能否自动发现 |
|:--|:--|:--|
| 文本模型 | 运行时拉上游 `/backend-api/models`（`ModelCatalog`，TTL 300s） | ✅ 能 |
| 图片模型 | 写死在 `internal/protocol/models.go` 的 `baseImageModels` | ❌ 不能 |

原因：上游 `/backend-api/models` **只返回文本模型**，图片模型（`gpt-image-2` 系列）
完全不在该响应里，无法通过任何接口自动发现，只能实测出图确认后写死。

### 上游文本模型（动态，无需改代码）

上游 `/backend-api/models` 加了模型，`/v1/models` 下次刷新（TTL 300s）
就会出现，**不需要动代码**。所以这里列的是某个时间点的快照，不是白名单：

| 时间点 | 数量 | 变化 |
|:--|:--|:--|
| 2026-10 初 | 11 | `auto`、`gpt-5-3-mini`、`gpt-5-6{,-mini,-t-mini,-t-mini-mini}`、`gpt-6{,-mini,-t-mini,-t-mini-mini}`、`research` |
| 2026-10-27 实测 | 13 | 上游新增两条 alpha 占位项 |

**判断「是不是模型丢了」的正确方法**：看 `/v1/models` 返回的数量对不对，
而不是逐个比对名字 —— 上游改名/增删模型是常态。

若数量明显偏少（例如只剩图片模型），多半是 `ModelCatalog` 的 fetcher
没注入（见下方「踩过的坑」），不是上游删模型。

### 踩过的坑

`NewModelCatalog(accounts, nil)` 的第二个参数是 fetcher。传 `nil` 不会报错，
只会让目录永远为空 —— 表现为 `/v1/models` 只有图片模型，文本模型全部消失。
`TestModelCatalogNilFetcherYieldsEmpty` 固定住这个行为，让漏注入在评审时可见。
