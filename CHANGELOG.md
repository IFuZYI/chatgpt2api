# Changelog

## 2.2.2 - 2026-10-27

代码审查发现的 2 个 critical + 1 个 silent-data-loss 修复。

### 修复

+ **[严重] 刷新账号仍会崩溃 —— v2.2.1 修错了地方。**
  v2.2.1 归因于 `statusMeta` 查表（那个也是真的，但用户撞不上）。
  真实原因：安全修复 `42dcbbd` 为防凭据泄漏把 `items` 从刷新进度结果里
  移除了，**前端从未跟着改** —— 仍在读 `progress.result.items`。

  链路：`setAccounts(undefined)` → 下一次渲染 `accounts.filter` 抛
  `Cannot read properties of undefined` → 整页崩溃。
  v2.2.1 加的 error boundary 只是把它降级成内联错误面板，页面依然不可用
  （实测：刷新后 `页面出错了` 出现）。

  修复：两条刷新路径都不再从进度载荷取列表，改为 `await loadAccounts(true)`
  重新拉取（走 `GET /api/accounts`，那里有脱敏）。

+ **[严重] `POST /api/settings` 明文回传密钥 —— v2.2.1 引入。**
  为修 WebDAV「留空不改密码」，我把已存密码注入了 `Update` 的返回值；
  但该端点返回的是 `s.Get()`（未脱敏），而 GET 是脱敏的。
  于是每次保存设置都把 `webdav_password` / R2 密钥 / AI key 明文送回浏览器，
  并被前端 `normalizeConfig` 存进 React state。

  修复：`POST /api/settings` 也走 `maskConfigSecrets`，并加 `no-store`。

+ **[严重] 保存任意设置会静默抹掉 R2 密钥与 AI key。**
  与 WebDAV 密码同一类 bug，但**没有校验错误** —— 更难发现：某天备份
  突然连不上，或 AI 审核失效，却没有任何提示。

  根因：前端 `saveConfig` 每次全量提交所有 section，脱敏后
  `secret_access_key` / `passphrase` / `api_key` 在响应里不存在 → 回传空串
  → 后端只做 normalize，不保留旧值。`ai_review` 更是完全没有归一化，
  连 `has_api_key` 都被持久化进 `config.json`。

  修复两端：
  - 后端：抽出 `preserveMaskedSecrets()`，`backup` 与 `ai_review` 都按
    「留空 + `has_*` = 不修改」处理，并统一剥掉 `has_*` 防止落盘；
    新增 `normalizeAIReview()` 白名单重建
  - 前端：抽出 `withSecretFlags()`，两处 payload 都带上 `has_*`

### 修复（次要）

+ 两处进度轮询的卸载守卫只 `clearInterval` 不 `resolve` —— 调用方的
  `finally` 永不执行，`refreshingTokens` / `isRefreshing` 卡在 true，
  刷新控件永久禁用。改为与另一处轮询一致地 `resolve`。
+ `normalizeStatusLabel` 是死代码（徽章仍渲染原始值）—— 已接上。
+ `statusMeta` 查表兜底（v2.2.1 已加）保留，它是真实的第二道崩溃源。

### 验证

- 单账号刷新：修复前 `页面出错了`，修复后正常
- `POST /api/settings` 响应：不含任何明文密钥，只有 `has_*`
- 全量保存后 R2 密钥与 AI key：保留 ✓，`has_*` 未落盘 ✓
- 8 包 + 2 包 `-race` + 67 项 compose 契约 + 46 前端测试全绿

## 2.2.1 - 2026-10-27

两个用户报告的故障修复。

### 修复

+ **[高危] 刷新时整页崩溃，显示「This page couldn't load」。**
  项目**没有 `error.tsx` / `global-error.tsx`** —— 任何渲染期异常都会让
  Next.js 用默认错误页替换整页，用户只能刷新，操作全丢。

  具体触发点：账号行渲染用
  `const status = statusMeta[account.status]; const StatusIcon = status.icon;`，
  而 `statusMeta` 只有 4 个键（正常/限流/异常/禁用）。上游返回任何其他值
  （带空格、空串、新增枚举）时索引是 `undefined`，紧接着读 `.icon` 抛
  TypeError —— 刷新后数据来自上游，最容易命中。

  修复三层：
  1. 新增 `error.tsx`（页面级）与 `global-error.tsx`（根布局级兜底，
     自带 `<html>`/`<body>`），把整页崩溃换成「局部报错 + 重试」，
     并显示错误摘要便于反馈
  2. 新增 `resolveStatusMeta()`：未知状态兜底为「禁用」样式，
     并把原始值展示出来（让人能看出是哪个状态没识别）
  3. 加固两处进度轮询 `setInterval`：加卸载守卫与 30 分钟超时；
     `onUpdate` 抛错不再让 Promise 悬挂（原来会让界面永远卡在
     「正在刷新」）

+ **[高危] 设置保存误报「启用 WebDAV 图片存储后必须填写 WebDAV 密码」。**
  这是 v2.2.0 引入的回归：脱敏把 `webdav_password` 从
  `GET /api/settings` 换成 `has_webdav_password`，但**只改了后端，没改
  前端回传**。前端拿到空密码 → 保存时原样回传空串 → 后端校验失败。
  用户明明早就配好了密码，只改了个无关设置也被拦。

  修复两端：
  - 后端：收到空 `webdav_password` 且 `has_webdav_password: true` 时，
    沿用磁盘上的旧密码（语义与 `proxy_runtime` 的 `_existing_cf_cookies`
    一致）
  - 前端：新增 `buildWebdavPayload()`，仅在密码框留空时回传 `has_*`
  - 密码框加提示：「已设置，留空表示不修改」—— 避免用户看到空框
    以为密码丢了

### 说明

+ WebDAV 启用状态下**无法清空密码**（会返回 400）。这是有意的：
  WebDAV 必须有密码。要停用请关闭 `enabled` 或切换 `mode`。

### 验证

- 真实浏览器（Playwright + Chromium）：6 个页面全部正常，零 pageerror
- 刷新中途离开页面 × 3 次：无崩溃
- 设置页点「保存」：不再误报
- 后端 8 包 + 67 项 compose 契约 + 43 前端测试全绿

## 2.2.0 - 2026-10-26

全量审计版本。**含 6 个安全修复 + 1 个数据丢失修复**，建议升级。

### 安全

+ **[高危] 配置类密钥明文回显。**
  `GET /api/settings` 返回 `Config.Get()`，其中含
  `backup.secret_access_key`、`backup.passphrase`、
  `image_storage.webdav_password`、`ai_review.api_key` —— 全部明文。
  同仓库对账号凭据（5 个字段）与 clearance cookie（`has_cf_cookies`）
  都做了脱敏，唯独配置里的密钥漏了：每次打开设置页都把这些密钥送进
  浏览器内存与代理日志。

  `GET /api/backups` 更严重 —— 这是被前端轮询的**列表**接口，
  同样回显 R2 密钥与备份口令。

  修复：新增 `maskConfigSecrets`，4 个字段替换为 `has_*` 布尔标记
  （与 `has_cf_cookies` 同构）。不修改入参，避免删掉内存里的真实密钥。

+ **[高危] 凭据文件权限 0644 → 0600。**
  `accounts.json`（含 access_token / refresh_token / password /
  totp_secret / session_token）与 `config.json`（含 auth-key = admin
  bearer、R2 密钥、备份口令、WebDAV 密码）都被写成 **0644** ——
  同机任何用户可读。`auth_keys.json` 更是直接拿到 admin bearer。

  更糟的是既有测试断言了 0644，把泄漏固化成了「预期行为」。

  修复：全部改为 0600，并更新那条测试。

+ **[高危] 刷新进度提前返回分支泄漏全池凭据。**
  `RefreshAccounts` 在 `len(unique)==0` 时构造的 result 里带
  `"items": ListAccounts()`，而进度接口原样返回。同函数末尾的非空
  分支此前已修，这个 early-return 漏了。

+ **[中危] `AIReview()` 返回共享 map。**
  调用方改返回值即绕过锁修改内部状态，与并发读者竞争。
  修复：返回 `deepCopyMap` 副本；`deepCopyMap` 同时补上 `[]any` 处理。

### 修复

+ **[高危] 账号池文件损坏导致全池数据丢失。**
  `accounts.json` 被截断/手改错一个字符时，`LoadAccounts` 返回
  `([], nil)` ——「损坏」与「空」不可区分。service 层再把错误当空池，
  下一次 `saveAccountsLocked()` 写入空数组，**全池 token 永久丢失且无备份**。

  修复两层：
  - storage：区分「文件不存在」（空，正常）与「解析失败」（返回错误）
  - service：加载失败时置 `loadFailed`，保存路径拒绝写入

  实测验证：撤销守卫后，48 字节的损坏文件被覆盖成 633 字节。

+ **[高危] `doGet` / `doPostJSON` 的 timeout 参数被完全忽略。**
  两个函数接收 `timeout` 却从不使用，而客户端级超时被显式设为 0。
  于是 `QueryTasks(5s)`、`GetConversation(60s)`、`/me(30s)`、
  `ListModels(30s)` 全部退化为**永不超时**。上游挂住一个连接时
  `generateWithRetry` 的 `defer release()` 永不执行，**账号槽位永久
  inflight**，号池逐步被蚕食。

  同时修 `DownloadImage`（无 deadline，body 中途停住即永久阻塞）与
  `UploadImage`（三次请求都用 `context.Background()`）。

+ **[高危] 5 处数据竞争（锁外读号池活 map）。**
  `ProxyFor`、`Stats`、`EditableFileAccessToken` 在 `RUnlock` 之后读
  号池里的**活对象**，而 `disambiguateAccountIDs` 在写锁下就地写同一个
  map —— Go 的 `fatal error: concurrent map read and map write` 不可
  recover，直接终止进程。`ProxyFor` 在每条上游请求上都会被调用。

  另修 `ModelCatalog.refresh` 的无锁字段读、`ImageTaskService.SetRunners`
  的无锁写。

+ **[中危] 合成闸门按请求限流而非按图，单请求可占 N×271 MiB。**
  `acquireComposite()` 在 `for` 循环**之外**只取一次，而循环内每张图
  各自最多 20M 像素（≈271 MiB），`images` 数量在 API 层无上限。
  改为循环内取闸门，并用闭包 + `defer` 保证错误路径也释放
  （原写法错误分支 `return nil, err` 会泄漏许可）。

+ **[中危] 进度条目无 TTL，只靠客户端轮询删除。**
  `refreshProgress` / `reloginProgress` 是 `sync.Map`，
  `GetRefreshProgress` 只在「读到 done」时删除。客户端拿完结果就关
  页面、或请求中途 panic 没走 Finish 路径时，条目永久残留 ——
  每个 `Progress` 带 Results 切片（每账号一条），261 个账号刷 200 次
  就是 5 万条常驻。

  修复：`Progress` 加 `UpdatedAt`，新增 `SweepStaleProgress` 与
  `StartProgressSweeper`（6 小时上限，接进 main 的启动/关闭链）。

+ **[中危] `Update` 先改内存后落盘。**
  写盘失败时调用方收到错误，但内存里已是新值 —— 用户被告知「保存
  失败」，后续任何一次成功写入却把它悄悄持久化。改为落盘成功后才
  更新内存。

+ **[中危] 前端定时器泄漏。**
  - 重登进度模拟的 2s 兜底分支不清 `setInterval`：`relogined >= 14` 时
    interval 永久每 150ms 调一次 `setProgress`
  - `progressRef` 从未赋值，卸载时的 cleanup 是死代码 —— 刷新中途
    离开页面，轮询会一直跑并 `setState`

  修复：兜底分支同时 `clearInterval`；新增 `unmountedRef` 让所有
  轮询循环在卸载后自行退出。

+ **[中危] 前端模块级无界缓存。**
  `b64BlobUrlCache` 与 `base64SizeCache` 以**整段多 MB base64** 为键，
  且 `URL.createObjectURL` 产生的 blob 从不 revoke —— 内存随浏览会话
  单调增长。改为 LRU（上限 32）并在淘汰时 revoke。

+ **[低危] 9 处后台 goroutine 无 recover。**
  `gin.Recovery()` 只兜 handler goroutine。刷新、重登、CPA 导入、
  sub2api 导入、图片任务等 `go func()` 在 handler 返回后继续跑，
  其中任何一个 panic 都会**终止整个进程**。新增 `goSafe` 统一包装。

+ **[低危] 其他**
  - `.dockerignore` 补 `*_test.go` 与 `.learnings`
  - 详情接口之外，设置与备份响应补 `Cache-Control: no-store`

### 文档

+ `AGENTS.md`：项目作业规范（5 条硬规则 + 操作要点，每条附实测来源）
+ `docs/deployment.md`：补「容器安全基线」与 `config.json` 权限的
  完整诊断（为什么只改目录权限不够，必须改文件属主）
+ `docs/account-api.md` / `docs/account-api-reference.md`：补 2FA 章节
  （只支持 TOTP）、`account_id` 章节、详情接口字段

### 迁移

+ **配置字段变更**：`backup.secret_access_key`、`backup.passphrase`、
`image_storage.webdav_password`、`ai_review.api_key` 在 GET 响应里
  变成 `has_*` 布尔标记。回写时若不传该字段则保留旧值。
  `testdata/golden/admin_settings.json` 已记录这处**有意偏离** Python
  契约的说明。

+ 已有账号在首次加载时自动回填 `account_id`，无需人工操作。
  JSON 与 SQL 后端都会持久化该字段；SQL 首次升级走 UPDATE 而非
  DELETE+INSERT，自增 id 保持不变。

## 2.1.0 - 2026-10-26

一次安全审计 + 两轮外部代码审计后的修复版本。**含 2 个安全修复，建议所有部署升级。**

### 安全

+ **[高危] 入站请求体无上限，单个小请求可打 OOM 单实例。**
  `decodeJSONBody` 用 `c.GetRawData()` 无限制读入，全仓库 `MaxBytesReader`
  使用为 0。`/v1/images/edits` 会把 base64 图片解出来做 mask 合成，而该解码
  发生在**号池配额与并发槽位准入之前**。

  实测（`GOMEMLIMIT=600MiB`、号池为空的实例）：1.5 MB 请求体（12000×12000
  图片）→ RSS 从 1.8 MB 涨到 461 MB（**+459 MB**）、耗时 **51.8 秒**，
  最终返回 429 —— 一张图都没生成。几个并发请求即可 OOM。

  修复：读之前套 `http.MaxBytesReader`（控制面 JSON 8 MiB、base64 图片
  端点 16 MiB），超限返回 413。重放同一攻击：**+8.6 MB / 0.069 秒**，
  内存放大降 53 倍、耗时降 750 倍。

  控制面取 8 MiB 而非更小：`POST /api/accounts` 导入的是整个号池的 JSON，
  实测 260 个账号的导出已有 880 KB（约 3.5 KB/账号）—— 1 MiB 只能装 302 个，
  一个 300 号以上的池子导出后无法再导入。

+ **[高危] 图片解码像素预算取自库阈值，远超进程可承受量。**
  `maxImagePixels` 原为 178,956,970 —— 那是 Pillow 的**硬错误阈值**
  （2 × `MAX_IMAGE_PIXELS`），不是本进程的内存预算。179M 像素 × 4 字节
  = 683 MB NRGBA，而 `compositeOneMask` 同时持有 `decoded` + `composited`
  + `alpha` + PNG 编码缓冲，峰值远超 683 MB。

  修复：默认 20,000,000 像素（约 4472×4472，放行 4096×4096），并给 mask
  合成加**并发闸门**（默认 1）—— 合成发生在号池配额与并发槽位准入之前，
  不设闸门时 N 个并发就是 N × 271 MB（实测 3 个并发把 512 MiB cgroup 钉在
  MemoryMax 上，两个半小时一个都没完成）。

  **预算与闸门必须联动**：2000 万像素单请求峰值 ~271 MB，两个并发即超过
  512 MB 容器，所以默认闸门为 1。可用两个环境变量按容器内存调整：
  `CHATGPT2API_MAX_IMAGE_PIXELS` / `CHATGPT2API_MAX_CONCURRENT_COMPOSITES`。

  更正一处早期说明：实测分配系数是 **14.2 字节/像素**而非 4 ——
  合成路径同时持有原始图、合成结果、缩放 mask、mask 灰度图与 alpha 五份缓冲。

  **有意偏离 Python**：Pillow 对 144M 像素只警告不拒绝，本版拒绝。

+ **[中危] multipart 上传绕过请求体上限。**
  `ParseMultipartForm` 是独立的 body 读取路径，不经过 `decodeJSONBody`，
  所以 `MaxBytesReader` 不生效。实测 41.9 MB multipart body 被接受，
  分配 274.8 MiB。修复：同一上限（16 MiB）单独套在 multipart 分支。

+ **[中危] PPT / PSD 端点漏在图片额度列表外。**
  `/v1/ppt/generations` 与 `/v1/psd/generations` 都收 `base64_images`
  （PSD 要求至少一张参考图），漏在列表外会让 >~750 KB 的参考图被 413
  —— 合法上传被误伤。

+ **[中危] 首次 401 就删除账号。**
  `shouldDeferInvalidTokenLocked` 在首次失效时 `last_invalid_at` 为空、
  返回 `false`，直接走删除路径。`auto_remove_invalid_accounts=true` 时
  一轮上游抖动会清空整个号池。修复：三道防线（新号 10 分钟宽限、
  `invalid_count <= 1` 一律推迟、30 秒短窗去抖）。

+ **[中危] 403 被无条件当作 token 失效。**
  上游 403 绝大多数是 Cloudflare 挑战而非死 token（`fingerprint.go` 自己
  就记录了这一点，`reset_session_status_codes` 默认就是 `[403]`）。
  一次指纹漂移或坏出口 IP 就能把整池健康账号标异常甚至删除。
  修复：401 一律算失效；403 仅当来自 `/backend-api/me` 才算。

+ **[中危] `client_task_id` 任意文件写入。**
  只做 `TrimSpace` 就进 `filepath.Join`，`../../../../x` 逃出 `data/files`，
  而 containment 检查在**写入之后**才跑。修复：字符集校验
  `[A-Za-z0-9_-]{1,128}`。

### 修复

+ `keepalive` 把 `access_token` 当作 `refresh_token` 传给回调 —— 续期完全空转。
+ 上游轮换返回的新 `refresh_token` 被丢弃。生产日志实证旧 RT 用几次后
  上游返回 `refresh_token_reused` 并**永久拒绝**，不保存新 RT 会耗光续期能力。
+ `last_token_refresh_error_at` 从未被写入 → 6 小时错误退避恒定失效。
+ `watcher.tick` 把 `expiring` 从 keepalive 名单剔除，导致可续期的账号
  被交给「不会续期」的路径杀死。
+ `nowLocal()` 写本地时间、`parseAccountTime` 按 UTC 读 → 所有时间窗口
  偏 ±7~8 小时（UTC+X 下退避永不过期，UTC-X 下退避立刻失效）。
+ `refresh_token` 被上游拒绝后只记日志，要等 AT 自然过期（最长 10 天）才
  发现。修复：立即触发密码重登。
+ 重登成功后不重置 keepalive 锚点 → 下一轮立刻又把它拉去换 AT。
+ `异常` 是终态：无 RT 的账号（260 个里 256 个）一旦变异常永不被拾取。
  修复：纳入重试，按 `invalid_count` 指数退避（5min → 6h 封顶）。
+ `DeleteImages` 对删除失败的条目也移除索引 → 远端对象永久不可删、
  本地文件被下次扫盘「复活」。
+ `removeEmptyDirs` 与 `SaveImageBytes` 竞争 → 实测 1200 次保存失败 73 次。
+ `image_retention_days` 只删本地文件：webdav 模式完全空转，both 模式留下
  「已过期但仍可读」的索引条目。且 `endDate` 少 1 天，保留期当天的图被误删。
+ `data/files/` 零保留策略，实测 ~2 MiB/任务永久累积。
+ `image_index.json` 损坏后 webdav 模式整库消失且无修复路径。
  修复：`.bak` 回退 + 损坏文件留档。
+ `image_tags.json` 非原子写 + 读在锁外（实测 4000 次读有 197 次读到空表）。
+ 流式生图的 error 事件排在 `data: [DONE]` **之后**，标准客户端读到 `[DONE]`
  就停止解析，看到的仍是干净的空成功。
+ 并行生图 goroutine 无 `recover()` → panic 杀进程；n=1 与串行路径同样裸奔。
+ 关闭时三个 `Close` 共用一个 30s 预算，`Images.Close` 恒为空转。
+ `chatCallOutcome.accountEmail` 数据竞争。
+ `ApplyRotatedTokens` 每次轮换泄漏一个槽位 + 在飞计数。
+ 刷新/重登进度对象永不淘汰。

### 新增

+ 时区矩阵 CI（UTC / Asia/Shanghai / America/Los_Angeles）+ `-race` 作业。
  此前 CI 不跑测试。时区矩阵不是可选优化：撤销时间字段修复后 UTC 下全绿，
  另两个时区才失败。
+ `CHATGPT2API_MAX_IMAGE_PIXELS` 环境变量。

### 测试

611 个测试，三个时区全绿；`-race` 干净。

## 2.0.1 - 2026-10-25

### 变更

+ [变更] **Python 后端从 main 移除，归档到分支 `archive/python-backend-2.0`**。
  共 99 个文件（`api/`、`services/`、`utils/`、`test/`、`scripts/`、`main.py`、
  `pyproject.toml`、`uv.lock`、`.python-version`）。为什么归档而非删除：
  `utils/sentinel.py` 是 Go 侧 fnv1a_32 实现的黄金基线，ADR-0003 的 golden
  也以它为准，需要能随时回查。详见 [docs/archive-python-backend.md](./docs/archive-python-backend.md)。
  `.audit/`（重构期验证记录）一并归档。

### 新增

+ [新增] **rt 换 at**：`POST /oauth/token` 用 `refresh_token` 换新 `access_token`
  （form-urlencoded + `grant_type=refresh_token` + `client_id`，
  与换 code 的 JSON body 形状不同，混用会 400）。上游轮换 rt 时采用新的，
  未返回则沿用旧的；非 200 或没有 access_token 一律报错并带状态码与 detail。
+ [新增] **`auto_relogin_after_refresh` 真正接线**：刷新发现失效账号（401/403）后
  自动密码重登。429 限流不触发（否则上游抖动会批量登录）。
  此前 `watcher.Start` 的第三个参数是 `nil`，keepalive 分支从未生效。
+ [新增] **密码重新登录（含 TOTP 两步验证）**。`/api/accounts/re-login` 从「未实现」改为真实现：
  authorize → sentinel（`fnv1a_32`，与 chat 的 SHA3-512 PoW **不同算法**）→ `password/verify`
  → TOTP → `mfa/verify` → OAuth `token` 交换 → 回写 access/refresh/id token。
  真实上游验证通过（access 1501 / refresh 196 / id 1369 字符）。
  新增 46 个测试，sentinel 的 fnv1a 常数与 TOTP 都钉在真实 golden 上（RFC 6238 Appendix B 向量）。
+ [新增] 跨平台二进制 Release：`.github/workflows/release.yml` 构建
  linux/darwin/windows × amd64/arm64（另附 linux/armv7），生成 `SHA256SUMS.txt` 并建 GitHub Release。
+ [新增] `config.BuildVersion` 支持 `-ldflags -X` 注入版本号 —— 单二进制没有 `VERSION` 文件可读，
  靠它让启动日志与 `--version` 显示正确版本。

### 修复

+ [修复] **流式生图失败被吞**：`/v1/images/generations` 流式路径在 `pool.Run` 失败时只写 `[DONE]`，
  客户端收到 HTTP 200 + 空流，无法区分「成功但 0 张图」与故障。
  改为在流内补 `{"error":{...,"type":"server_error"}}` 事件，与 `/v1/responses` 对齐。
+ [修复] **`SyncToWebDAV` 丢索引**：在网络循环前取快照、循环后整份写回，期间
  `SaveImageBytes` 新增的条目被抹掉。`webdav` 模式没有本地副本 → 图永久不可达（不可恢复）。
  改为只合并本次上传成功的条目，并在锁内重新读取索引。
+ [修复] **图片标签并发丢更新**：`loadTags`/`saveTags` 无锁而 `SetTags`/`DeleteTag` 是读改写，
  实测 50 个并发 `SetTags` 只留下 **0** 条。加 `tagsMu` 保护全过程。
+ [修复] **`/v1/images/edits` 的 `n` 静默夹取**：generations 越界返 422、edits 却把 5 夹成 4，
  同一文件两套行为。统一为校验报错（edits 返 400，对齐 Python `_parse_count`）。
+ [修复] **export 丢 `password`**：`BuildExportItems` 只导 token，导致 export → import 后
  每个账号都 `no_password_in_account`，密码重登对导出的号池失效。补上 `password` / `totp_secret`。
+ [修复] **`/api/accounts` 泄露密码**：密码重登上线后 `password` / `totp_secret` 成为有效凭据，
  列表接口却原样返回 —— 一次只读 admin 会话即可全量接管号池。改为脱敏，
  保留 `password_set` / `totp_secret_set` 布尔值供前端显示。
+ [修复] **`KeepaliveRefreshTokens` 两处语义错误**：
  (a) 回调签名是 `RefreshTokenFunc(refreshToken)`，实现却传 **access_token**
  —— refresh_token 才是换新 token 的凭据；(b) 拿到新 token 后**不回写**号池，
  keepalive 等于空转。修复后换 token 时同步搬别名链（旧 token 立刻失效，
  仍持有旧引用的调用方要能找到账号）。
+ [修复] **`invalid_password` 只靠英文子串匹配**：补 `code == "invalid_password"` 分支，
  消息匹配降为兜底。
+ [修复] **admin auth-key 出现在跟踪文件里**：`.codex/skills/chatgpt2api-search/SKILL.md`
  写着 `Authorization: Bearer chatgpt2api`，与部署实例的 `auth-key` 逐字相同。
  任何拿到仓库的人都能直接过 `RequireAdmin`。已改为占位符。
  18 个测试夹具 / golden 里的同形值一并改为 `test-key-not-real`。
+ [修复] **`config.example.json` 泄漏真实凭据**：模板曾携带一条真实在用的 SOCKS5 出口代理凭据
  （与部署 `config.json` 逐字节相同）与私有 WebDAV 主机名 / 用户名。
  已清除；**该凭据已推送过，需轮换**（修复 HEAD 无法追回已 clone 的副本）。
+ [修复] **全新克隆无法构建**：`.gitignore` 忽略了整个 `web_dist`，而 `.gitkeep` 未跟踪 ——
  `//go:embed all:web_dist` 在缺目录时直接编译失败
  （`pattern all:web_dist: no matching files found`）。
  改为只忽略目录内容、跟踪 `.gitkeep`，已验证全新克隆可 `go build ./...`。
+ [修复] **CI 从未成功构建过镜像**：`docker-publish.yml` 写着 `target: app`，
  而 Dockerfile 没有该 stage → `target stage "app" could not be found`。
  Dockerfile 最终 stage 命名 `AS runtime`，workflow 改为 `target: runtime`。
+ [修复] **`.dockerignore` 未排除根目录旧二进制**：规则写成 `/chatgpt2api`（带前导斜杠）没生效，
  56MB 的 ELF 白进构建上下文。同时补排除 `web/out`、`internal/web/web_dist`、
  `.superpowers`、`.audit` 等。上下文从 195MB 降到约 2MB。

### 文档

+ [文档] **README 重写**：修正错误的模型清单（原文写的 `gpt-5` / `gpt-5-1` 早已不存在，
  实际是 `gpt-5-6` / `gpt-6` 系列 + 4 个生图模型）、补上 `cp config.example.json config.json`
  这一步（缺了它 `docker compose up` 直接失败）、统一端口说明
  （源码运行 `:8000`、Docker 容器内 8080 / 对外 3000）、修正测试数（170 → 561）。
+ [文档] **`docs/deployment.md` 重写**：新增四种部署方式对比、反向代理（nginx / Caddy 的 SSE 配置）、
  存储后端、图片存储三模式、备份与加密、升级回滚、安全加固、排障、维护命令。
+ [文档] **ADR-0007 修正失实描述**：原文说清理用 `context.WithoutCancel`、看门狗用
  `time.AfterFunc`，代码里都不存在。改为如实描述。
+ [文档] **ADR-0009 重写**：原文把密码重登 / PPT-PSD / 网页搜索 / Responses 图片四项列为「未实现」，
  实际全部已落地；测试数还写 170。改为「曾经的空白，现已补齐」+「遗留的已知限制」两张表。
+ [文档] **`SaveImageBytes` 注释**与 ADR-0011 矛盾（说「WebDAV 失败必须让整次保存失败」，
  实际 `both` 模式会降级）—— 按模式分列写清。
+ [文档] **`runImageTask` 注释**自相矛盾（写「不能用 Background 裸跑」，下一行就是
  `context.Background()`）—— 改为如实记录「后台任务不可取消」这一已知限制。
+ [文档] 修正路由数：Python 78 条 → Go **77** 条（删除了 `/api/third-party-apps`）。

### 变更

+ [变更] 默认模板 `config.example.json` 对齐代码默认值：
  `image_storage.enabled=false` / `mode=local`、`proxy_runtime.skip_ssl_verify=false`。
  此前模板带 `enabled=true` + `mode=webdav` 且密码为空，导致照模板部署后
  **任何** `POST /api/settings` 都返回 400（连改日志级别都失败）；
  `skip_ssl_verify=true` 则让每个部署者默认关闭上游 TLS 校验。

### 已知问题

+ ~~`auto_relogin_after_refresh` 仍未接线~~ → 已接线（2.0.1）。
+ 后台图片任务不可取消：`TaskContext` 不带 ctx，`Close` 只能等满超时。
+ 刷新 / 重登的进度对象不淘汰（`CleanRefreshProgress` / `CleanReloginProgress` 无调用点）：
  每次 admin 操作永久保留一份号池副本，重启才回收。
+ 可编辑任务与 `data/files/` 无保留策略、无删除路由、无配额（对比图片任务有保留期）。
+ 入站请求体无大小上限（`decodeJSONBody` 用 `c.GetRawData()`，全仓库无 `MaxBytesReader`）。
+ 备份归档默认不加密（`backup.encrypt` 默认 `false`），而归档内含号池全部凭据。
+ 图片标签写入非原子（`saveTags` 直接 truncate + write），崩溃中途会丢全部标签。
+ 上游错误文本未脱敏，会跨到普通用户并写入日志。
+ 匿名对话链路受上游限制（见 ADR-0008）。
+ codex 图片链路的响应事件格式未经真实上游验证（见 ADR-0010）。

## 2.0.0 - 2026-10-23

### 重大变更：后端从 Python/FastAPI 全量重写为 Go/Gin

后端 17,642 行 Python 重写为 Go，单二进制部署。前端与 `data/` 存量数据零迁移。

+ [重构] 后端全量重写为 Go 1.27 + Gin，单静态二进制（CGO_ENABLED=0），镜像改用 distroless。
+ [重构] 前端静态资源用 `go:embed` 打进二进制，运行时可用 `CHATGPT2API_WEB_DIST` 覆盖。
+ [重构] 上游 HTTP 客户端改用 `bogdanfinn/tls-client`，复刻 curl_cffi 的浏览器指纹（TLS + header 顺序）。
+ [重构] 备份加密从 shell out `openssl enc` 改为原生 `crypto/aes` + PBKDF2，产物仍与 OpenSSL 双向兼容。
+ [优化] 图片并行生成改用 goroutine + 每账号 channel 信号量，槽位获取/释放成对，消除泄漏。
+ [优化] 文本对话缓存改为进程内 LRU + in-flight 去重，同 key 并发只算一次。

### 兼容性

+ 全部 77 条 HTTP 路由、三套错误信封（管理面 / OpenAI / Anthropic）、`config.json` 语义、`data/` 文件格式逐字保留。
+ 认证哈希保持裸 SHA-256（无盐），存量 `data/auth_keys.json` 直接可用。
+ `logs.jsonl`、`accounts.json`、`image_tasks.json`、`cpa_config.json`、`sub2api_config.json` 字段集不变。

### 修复

+ [修复] `download_images_zip` 恢复 WebDAV 回退分支（Python 版被同名函数覆盖，WebDAV 模式下 zip 下载实际不可用）。
+ [修复] 匿名链路不再发送空 `Authorization: Bearer `（空 token 会被上游判为无效凭据）。
+ [修复] `/v1/models` 补齐全部文本模型（`gpt-6` / `gpt-5-6` / `research` 等 11 个）。
  根因是 `NewModelCatalog` 的 fetcher 传了 nil，目录永远为空，接口只剩图片模型。
  文本模型改为运行时拉上游 `/backend-api/models`（TTL 300s，账号变更即失效），
  新增文本模型不再需要改代码。
+ [新增] 图片模型清单补齐 `gpt-image-2.5` / `gpt-image-2.5-flare` / `gpt-image-2.5-sunburst`。
  这三个 slug 不在上游 `/backend-api/models` 返回里，实测出图确认后收录；
  模型名直接作为上游 slug 透传，不走 `default_upstream_model_name` 映射。
+ [修复] `conversation/init` 改为 POST + body（原用 GET，稳定 400，导致账号刷新拿不到 email/quota）。
+ [修复] 图片下载补 `Authorization`（`/backend-api/estuary/content` 带签名仍要求登录态，否则 404）。
+ [修复] PoW 三段静态 JSON 的裁剪规则（`static_3` 必须保留尾部 `]`）。
  少一个字符会让 sentinel token 格式正确但上游稳定 500。
+ [修复] 图片链路改走 `/backend-api/f/conversation`（prepare 换 conduit_token），
  原走文本端点导致整条链路不可用。
+ [修复] 刷新账号后回写 `email`/`quota`/`type` 等字段（原只计数不回写，刷新「成功」但数据全空）。
+ [修复] 图片成功计数改为按实际出图判定（原无条件记 success，失败也扣额度）。
+ [修复] codex 图片模型（`codex-gpt-image-2` 及 `plus-`/`team-`/`pro-` 别名）改走
  `/backend-api/codex/responses`，按 `source_type=codex` 选号。原被塞进 web 的
  `/f/conversation` 链路，导致 4xx/5xx 或扣错额度，而 `/v1/models` 一直宣告可用。
  响应事件格式按 Python 解析器复刻、未经真实上游验证，见
  `docs/adr/0010-codex-responses-response-shape-unverified.md`。
+ [修复] 图片任务服务补齐关闭语义：`Close(ctx)` 等待在途任务 goroutine 退出后再落盘。
  原实现进程退出时不等待，`image_tasks.json` 可能写到一半（重启后任务损坏或丢失）。
  优雅关闭顺序为 `server.Shutdown` → 停后台调度器 → `Tasks.Close(30s)`；超时记日志不静默。

### 已知问题

+ 匿名对话链路（`/backend-anon/conversation`）当前被上游限制：Python 返回 403、Go 返回 500。
  payload 与 header 已逐字段核对一致，非实现缺陷。详见 `docs/adr/0008-anonymous-conversation-unavailable.md`。
+ ~~密码重新登录返回「未实现」~~ → 2.0.1 已实现（含 TOTP）。
+ ~~可编辑 PPT/PSD 生成、网页搜索、Responses 图片工具调用尚未接入（返回 501）~~ → 2.0.1 已全部接入。
+ codex 图片链路的**响应事件格式**未经真实上游验证（请求侧已逐字对齐 Python）：
  若上游改用分片事件（`partial_image_b64`）而非一次性 `result`，会报
  `No image result found in response`。详见 `docs/adr/0010-codex-responses-response-shape-unverified.md`。

## 1.8.0 - 2026-07-28

+ [新增] 新增默认请求上游模型名称和默认思考强度配置，支持在设置页面修改，并可通过模型名的 `-standard`、`-extended`、`-max` 后缀覆盖思考强度。
+ [修复] 新增没出图也移除本地对话的配置，支持在图片生成失败、超时或仅返回文本时异步隐藏对应对话记录。
+ [修复] `/v1/models` 汇总各账号类型的官方模型列表，文本请求按模型权限选择账号。
+ [修复] 过滤隐藏、非最终频道及发往内部工具的助手消息，避免搜索指令和推理内容泄漏到 API 输出。
+ [修复] 修复输出清洗误删代码和命令中标点前空格的问题。
+ [修复] 为图片生成 SSE 流增加可配置的硬超时上限，避免上游长连接长时间挂起。
+ [优化] 数据库存储改为增量同步，仅新增、更新或删除发生变化的记录，保留未变记录的 ID。

## 1.7.0 - 2026-07-05

+ [移除] 移除注册功能、防滥用机制导致封禁GitHub账号。

## 1.6.0 - 2026-07-04

+ [修复] 修复sub2api导入问题。
+ [修复] 修复前端404、405问题。
+ [新增] 新增出图后删除对话记录功能。
+ [调整] Pro号不再按无限额度处理、约每天1000张。

## 1.5.0 - 2026-06-13

+ [新增] 新增 WARP / Privoxy / FlareSolverr 清障方案，注册遇到 Cloudflare 拦截后可刷新 clearance 并重试。
+ [新增] 新增 `outlook_token` 邮箱池，支持 Outlook/Hotmail 注册验证码读取。
+ [新增] 新增网页搜索兼容接口、图片编辑 mask 和图片任务相关能力。
+ [优化] 更新 sentinel/PoW 获取方式，提高上游请求兼容性。
+ [优化] 调整代理优先级和注册请求重试逻辑。

## 1.4.1 - 2026-06-03

+ [新增] 账号刷新改为异步模式，支持前端轮询刷新/重新登录进度。
+ [新增] 号池管理页面新增重新登录功能，支持密码登录恢复异常账号。
+ [新增] 刷新后自动重新登录异常账号（可在设置页开启）。
+ [新增] 图片生成支持并行模式，多张图片使用独立线程和账号同时生成。
+ [新增] 图片轮询超时自动换账号重试（最多4次），连接超时同账号递增等待重试。
+ [新增] 图片二次确认机制与先check再hit可配置化，关闭后可跳过等待直接返回结果。
+ [新增] 图片任务进度追踪，显示当前生成步骤（上传/预热/获取token/生成中等）。
+ [新增] 图片超时后续轮询功能，前端显示"继续等待"按钮。
+ [新增] 设置页新增图片二次确认、超时等待时间、自动重新登录等配置项。
+ [优化] 优化生图页面滚动加载性能，图片懒加载、会话切换滚动位置保存与恢复。

## 1.4.0 - 2026-05-31

+ [新增] 新增AI生成可编辑PSD文件逆向。
+ [新增] 新增AI生成可编辑PPT文件逆向。

## 1.3.1 - 2026-05-30

+ [新增] 新增ChatGPT搜索调试、Skills。

## 1.3.0 - 2026-05-30

+ [新增] 新增ChatGPT搜索接口逆向。

## 1.2.4 - 2026-05-30

+ [新增] 添加聊天补全缓存与重复请求合并。
+ [新增] 新增无限画布一键跳转功能

## 1.2.3 - 2026-05-29

+ [新增] 新增账号级代理。
+ [修复] 修复503异常信息、前端邮箱换行问题。

## 1.2.2 - 2026-05-29

+ [新增] 新增Codex链路生图、支持2k,4k。
+ [新增] 支持RT刷新账号信息。

## 1.2.0 - 2026-05-28

+ [新增] 当前版本基线，包含 Web 面板、画图、号池管理、注册机、图片管理、日志管理和设置能力。
+ [优化] 优化注册机效率，成功率大幅提高。
+ [优化] 优化生图页面配置选项。
