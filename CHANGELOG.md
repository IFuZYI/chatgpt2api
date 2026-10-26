# Changelog

## 2.3.3 - 2026-10-26

账号列表信息层级重排 + 移动端布局修复。

### 变更

+ **账号信息移到第一列，移除 token / 来源 / 创建时间三列。**
  之前 token 占第一列（2000+ 字符的 JWT，占宽且无法快速识别），
  账号信息在第 6 列 —— 用户要扫一眼看「这是哪个号」，却先看到一串 token。

  现在第一列是**账号信息**：邮箱为主行、`account_id` 前 8 位为副行
  （可复制完整值）。token / 来源 / 创建时间移入详情弹窗 ——
  它们占宽且不属于「扫一眼」的范畴。

  列数从 11 降到 8，横向滚动需求明显下降。

+ **行左侧加状态色条。**
  正常=绿、限流=琥珀、异常=玫红、禁用=灰。
  58 个异常账号在一屏里能直接按颜色分组，不用逐行读状态徽章。

### 修复

+ **移动端统计卡片单列堆叠，占满整屏。**
  dogfood（390×844 视口）实测：6 张卡片单列堆叠，账号列表要滚 3 屏
  才出现 —— 而列表才是这页主体。

  改为 `grid-cols-2`（移动）→ `md:grid-cols-3` → `xl:grid-cols-6`。
  实测高度从 3 屏降到约 1 屏。

+ `Account` 类型缺 `account_id`（详情功能加了这个字段但类型没跟上）。

+ 删除两个已无调用者的函数（`maskToken` / `displayAccountSource`）。

### 新增

+ `src/lib/account-columns.ts` —— 列定义与响应式网格抽成纯数据，
  用 `node --test` 断言「账号信息必须在第一列」「列表不该有 token 列」
  「移动端必须多列」这类需求。肉眼看 JSX 容易漏。

### 验证

真实浏览器截图（Playwright + Chromium）：
桌面 1440×900 与移动 390×844 两个视口，确认列顺序、状态色条、
卡片网格全部生效。

## 2.3.2 - 2026-10-26

Docker 部署全面优化。

### 修复

+ **`docker-compose.local.yml` 与 `docker-compose.warp.yml` 的
  `config.json` 同样会 `permission denied`。**
  2.3.1 只修了主 compose 的 `init-data`，而另外两个也挂 `config.json`
  却只 chown 了 `data/`。修复：三个 compose 全部处理。

+ **三个 compose 都没有安全加固。**
  补齐 `user: "65532:65532"`、`cap_drop: [ALL]`、
  `security_opt: no-new-privileges:true`。

+ **容器日志无大小限制。**
  docker 默认无限增长；本服务每 5 分钟刷一次账号，日志量不小，
  单机跑几个月会写满磁盘。补齐 `json-file` + `max-size: 10m` +
  `max-file: 3`（含 warp 的 privoxy / flaresolverr）。

+ **`.dockerignore` 未排除测试文件与本地日志。**
  `*_test.go`（1 MB，含测试夹具常量）与 `.learnings/` 会进构建上下文。

### 新增

+ **`scripts/test-compose.py`** —— Docker 部署的契约测试，43 项检查，
  每个用例对应一个真实部署缺陷（不是最佳实践清单）：

  1. 每个 compose 的 `init-data` 必须 chown `config.json`，且真的挂载它
  2. `app` 必须显式非 root + `cap_drop ALL` + `no-new-privileges`
  3. 每个长期运行的服务必须有日志大小限制
  4. `/app/data` 必须可写（唯一需要持久化的目录）
  5. `.dockerignore` 必须挡住 `data` / `config.json` / `.learnings` / `*_test.go`
  6. Dockerfile 最终层只能 COPY 二进制与 VERSION，且切到 nonroot

  改动前 19 项失败 → 改动后 43 项全过。

### 文档

+ `docs/deployment.md` 补「容器安全基线」章节与 `config.json` 权限的
  完整诊断说明（为什么只改目录权限不够，必须改文件属主）。

### 验证

真实容器端到端：`config.json` 属主 `root:root` → `65532:65532`；
容器 `healthy`；设置保存 `200` 且配置真的落盘；加固后 5 个关键端点全 200；
`docker inspect` 确认 User / CapDrop / SecurityOpt / LogConfig 全部生效。

## 2.3.1 - 2026-10-26

code review 后的修复版。**含 2 个安全修复**，建议升级。

### 安全

+ **[高危] 刷新进度接口泄漏全池凭据。**
  `GET /api/accounts/refresh/progress/:id` 返回的 `result` 里带
  `"items": ListAccounts()` —— 刷新**一个**账号会泄漏**全池**的
  `password` / `totp_secret` / `refresh_token` 原文，**包括这次根本没被
  刷新的账号**（实测确认）。

  修复：进度结果不再包含 `items`（进度契约只需计数与错误）。
  重登进度同样处理。

+ **[中危] `POST /api/accounts/oauth/finish` 未脱敏。**
  `AddAccountItems` 返回号池原始记录，该端点直接透传。

  修复：抽出 `maskItems` helper（同时处理 `[]map[string]any` 与 `[]any`
  两种形态），四个端点统一使用 —— 之前每个端点各写一遍类型断言，
  正是这样漏掉了两个。

### 修复

+ **`config.json: permission denied` —— Docker 设置页无法保存。**
  `config.json` 以 `root:root 644` 单文件 bind mount 挂进容器，而容器以
  uid 65532 运行。两个写入点都失败：创建 `.tmp` 失败（`/app` 目录不可写）
  + 回退就地写失败（文件属主不是 65532）。

  修复两层：
  1. compose 的 `init-data` 现在也 chown `config.json`（单文件挂载即可，
     实测可行且会改到宿主文件）
  2. 错误信息带上可操作指引（哪个文件、宿主侧 `chown` 命令、替代方案），
     而不是只报 `permission denied`

  实测：修复前 `400 permission denied`，修复后 `200` 且配置真的落盘。

+ **设置页默认模型文案仍写 `gpt-5-5`。**
  后端回退值早已改为 `auto`（`gpt-5-5` 已从上游模型列表消失），但前端
  placeholder、说明文案、以及 store 的两处回退值仍是旧值。后果：用户以为
  当前值就是 `gpt-5-5`，保存时把它写回 `config.json` 覆盖掉 `auto`。

  修复：抽出 `defaultUpstreamModelName()`，四处统一到 `auto`。

+ **前端全选是空操作、每次刷新列表清空选中。**
  2.3.0 把行 key 与表头判定改成 `accountRowKey`，但 `toggleSelectAll` 与
  6 处 reload 剪枝仍按 `access_token`。修复：抽出 `selectAllKeys` /
  `pruneSelection`，7 处全部迁移。

+ **磁盘上已重复的 `account_id` 修不掉。**
  `disambiguateAccountIDs` 第一轮对重复 id 的第二个账号没占位，第二轮又
  误判「已占位」而跳过 —— 永远不重新派生。修复：用 keeper 映射记录谁真的
  占位成功。

+ **`Detail()` 在锁释放后读号池活对象**（真实数据竞争）。
  改为读副本。

+ 详情接口加 `Cache-Control: no-store`（含凭据原文）。

### 文档

+ 修正两处漂移：样例标注（详情给原文、列表给脱敏）、400 错误信息措辞、
  `account-api.md` 重复的编号。

## 2.3.0 - 2026-10-26

账号稳定标识 + 详情弹窗。**含 1 个安全修复**，建议升级。

### 安全

+ **[中危] 4 个账号端点的响应泄漏凭据原文。**
  脱敏函数 `maskAccountCredentials` 原本只在 `GET /api/accounts` 一个 handler
  里调用，而 `POST /api/accounts`、`/api/accounts/update`、
  `/api/accounts/proxy`、`DELETE /api/accounts` 都直接返回
  `ListAccounts()` —— 实测响应含 `password` / `totp_secret` /
  `refresh_token` / `id_token` 原文（DELETE 一次返回 260 个账号全部凭据）。

  虽然都要 admin 权限，但列表接口刻意脱敏说明设计意图就是「不在批量读里
  给原文」；且前端 update 后会把 items 存进状态，等于把密码送进浏览器内存。

  修复：四个端点全部走脱敏；脱敏范围从 2 个字段扩到 **5 个**
  （补 `refresh_token` / `id_token` / `session_token`）；
  并改为**复制后处理**（原实现就地 `delete` 会改写入参 ——
  若该 map 与号池内存共享，密码被删后重登会失败）。

  新增**端点级集成测试**：经真实 router 逐个打 5 个返回账号列表的端点。
  这比只测脱敏函数有价值 —— 后者测不出「handler 忘了调用它」。

### 新增

+ **账号详情弹窗。** 号池管理每个账号行新增眼睛按钮，点开显示 5 组信息：
  基本信息 / 凭据（密码、2FA）/ 令牌与有效期 / 运行状态 / 诊断。
  凭据默认遮罩，点「显示」展开，可复制。

  派生字段：`access_token_expires_in`（**-1 = 非 JWT，无法判断**，
  与「已过期」严格区分）、`access_token_expires_at`、`proxy_source`
  （account/global/direct）、`proxy_effective`、`has_*` 布尔值。

  诊断组展示 `invalid_count` / `last_invalid_at` / `last_refresh_error` /
  `last_token_refresh_*` / `default_model_slug` —— 这些后端一直在返回但 UI
  此前完全不展示，而它们正是「这个号为什么失效」的答案。

+ **`GET /api/accounts/detail`** —— 单账号详情，返回凭据原文（与列表接口
  刻意不同）。同时接受 `access_token` 与 `account_id` 两个参数名。

+ **`account_id`：UUID 形态的稳定标识，终身不变。**
  此前唯一可用的引用键是 `access_token`，而它会变：RT 换 AT（keepalive，
  每 3 天一次）与密码重登都会换掉它。

  后果（实测）：
  - 前端 React key 用 token，轮换后 DOM 被重建，**选中态与展开态丢失**
  - SQL 后端把旧行 DELETE、新行 INSERT，自增 id 从 1 变成 3
  - 外部系统记下的 token 引用在轮换后失效（`aliases` 只在内存，重启即丢）

  生成规则：新账号 UUIDv5 确定性派生；已有 `account_id` 原样保留；
  历史账号按种子确定性回填。**确定性是关键** —— 随机生成会让每次重启
  换一批 ID，等于没有稳定标识（实测 260 个账号重启前后 ID 完全一致）。

  唯一性由 `disambiguateAccountIDs` 在**插入点**保证，并优先保留已持久化
  的 id（可能已被外部引用）。

### 修复

+ **`account_id` 同种子碰撞导致按 ID 查询解析到不确定的账号。**
  派生只用「第一个非空字段」做种子，两个账号共享 `user_id`/`email` 时
  会派生出**完全相同**的 id；而 `findByAccountIDLocked` 返回 map 迭代中的
  第一个匹配 —— Go 的 map 顺序随机。实测：连续两次查询分别得到 `at-1`
  与 `at-2`。

  修复时踩了三个坑（记录在 `AGENTS.md`）：把 token 纳入种子会破坏轮换
  稳定性；`persisted` 用「id 非空」判断会把刚派生的也当成已持久化；
  最终在插入循环**之前**快照 pre-existing 账号才通过。

+ 前端选中态在 token 轮换后丢失。`React key` 与 `selectedIds` 从
  `access_token` 改为 `account_id`（批量操作时再映射回 token）。

+ `POST /api/accounts/update` 无法补 `totp_secret` / `password` / `email`
  —— 白名单原本只有 4 个字段，漏了 TOTP 的账号只能重新上传整个对象。

### 迁移

+ 已有账号在首次加载时自动回填 `account_id`，无需人工操作。
  实测 260 个账号全部回填成功，重启前后 ID 完全一致，唯一性 260/260。
+ JSON 与 SQL 后端都会持久化该字段（SQL 的 `data` 列存整个对象，无需改表）。
  SQL 首次升级走 UPDATE 而非 DELETE+INSERT，自增 id 保持不变。

### 文档

+ `AGENTS.md`：项目作业规范（5 条硬规则，每条附实测来源）
+ [账号接口](docs/account-api.md) 与 [格式参考](docs/account-api-reference.md)：
  补 2FA 章节（只支持 TOTP）、`account_id` 章节、详情接口。

## 2.2.1 - 2026-10-26

补 2FA 文档与接口缺口。

### 修复

+ **`POST /api/accounts/update` 无法补 `totp_secret` / `password` / `email`。**
  白名单原本只有 `type` / `status` / `quota` / `proxy`。注册机常先传 token、
  事后才发现账号开了 2FA —— 此时只能重新上传整个账号对象，既笨拙又容易
  把已有字段覆盖成空。三个字段已加入白名单（只改传了的，没传的保持不变）。

### 文档

+ 新增 [2FA / TOTP 章节](./docs/account-api-reference.md#65-两步验证2fa--totp)：
  - 支持与不支持的 2FA 类型（**只支持 TOTP**，邮箱/短信/硬件密钥不行）
  - `totp_secret` 的格式与解析容错（大小写/空格/连字符/`=` 填充实测等价）
  - 从哪些地方取值（认证器 App、`otpauth://` 二维码的 `secret=` 参数）
  - 漏了 `totp_secret` 的后果：导入不报错，但 AT 失效后**无法自动恢复**
+ 操作文档补「2FA 要点」速查表。

## 2.2.0 - 2026-10-26

账号接口面向注册机场景的整理。**含一处行为变更**（上传默认不再同步刷新）。

### 新增

+ **账号级代理真正生效。** 账号记录里的 `proxy` 此前只被显示层读取
  （`GetProfile` → `RuntimeStatus` → `/health`），7 处上游客户端全部只用
  `config.json` 的全局 `proxy` —— 给每个账号配不同出口 IP 在数据上成立、
  在行为上无效。

  现在优先级为：**账号 `proxy` → 全局 `proxy` → 直连**。覆盖账号信息刷新、
  文本对话、图片生成/编辑、搜索、PPT/PSD、模型目录拉取。
  密码重登仍走全局（登录时只有邮箱密码，无法反查账号代理）。

+ `POST /api/accounts/proxy` —— 批量设置代理。支持统一代理
  （`access_tokens` + `proxy`）与逐个映射（`proxies` 对象，空串=清空回退全局）。

+ [账号接口](./docs/account-api.md) 与 [账号接口格式参考](./docs/account-api-reference.md)
  两份文档：操作流程 + 字段级格式（完整账号对象、枚举、错误码、上限）。

### 变更

+ **上传账号默认不再同步刷新。** 原来 `POST /api/accounts` 会给每个账号
  打 3 次上游请求（me → conversation/init → accounts/check，**串行**）。
  500 个账号要串行等数分钟，上游还会看到 500 个新会话。

  现在立即返回，刷新交给后台 watcher。需要上传前验证 token 的调用方传
  `?refresh=1` 恢复旧行为。

  返回体新增 `refresh_deferred`（交给后台刷新的数量）。

### 修复

+ 上传时同一 token 在 `tokens` 与 `accounts` 里各计一次，导致
  `refresh_deferred` 翻倍（实测上传 1 个账号报 2）。已去重。
+ `FetchAccountInfo` 注释写「并发查 me / conversation/init / accounts/check」，
  实际是**串行**三次请求。已按实现修正注释。

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

## Unreleased

### 变更

+ [变更] `default_upstream_model_name` 的默认值从 `gpt-5-5` 改为 `auto`。
  该键决定 `gpt-image-2` 这个通用出图入口实际调用哪个上游模型。

  改的原因：`gpt-5-5` 已不在上游 `/backend-api/models` 返回的列表里（实测）。
  它目前仍被上游接受（搜索链路用它仍能正常返回），但作为**默认值**指向一个
  已不对外列出的模型是脆弱的 —— 上游随时可能真正下线它，而那时所有用默认值
  的生图请求会一起失败。`auto` 是上游始终提供的兜底项，语义也更贴合
  「让上游自己挑」。

  改动位置：
  - `internal/config/config.go` 的回退值
  - `internal/upstream/image_chain.go` 的空值兜底
  - `config.example.json`（模板）
  - 相关测试断言（`TestDefaultsMatchPythonConfig` 等 3 处）

  注意：这是**有意偏离 Python 默认值**的唯一一项，已在测试注释里写明理由。

  已有部署不会自动改 —— `config.json` 里若显式写着 `gpt-5-5`，它继续生效。
  想跟随新默认值，在 Web 面板设置页改，或把它从 `config.json` 删掉。

## 2.0.2 - 2026-10-25

修三个部署阻断。2.0.1 的镜像与 compose 文件都无法直接按 README 跑通。

### 修复

+ [修复] **WARP 部署完全跑不起来**：`docker-compose.warp.yml` 的 `init-config`
  容器原本执行 `python3 /app/scripts/init_proxy_config.py`，但 `scripts/` 已随
  Python 后端归档到分支 `archive/python-backend-2.0`，`main` 上不再存在。
  改为跑同一二进制的子命令 `--init-proxy-config`（见下方「新增」）。
+ [修复] **`init-config` 的 command 被追加到 ENTRYPOINT**：Dockerfile 里是
  `ENTRYPOINT ["/app/chatgpt2api"]`，若 `command` 再写一遍二进制路径，实际执行
  变成 `/app/chatgpt2api /app/chatgpt2api --init-proxy-config`。
  `flag.Parse` 遇到位置参数即停止解析，子命令不生效，程序掉进正常启动流程
  并报 `auth-key 未设置` —— 报错完全不指向真正原因。
+ [修复] **默认 compose「能启动但存不了数据」**：镜像以 distroless `nonroot`
  （uid 65532）运行，而 `./data` 由 Docker 首次创建为 `root:root 755`。
  结果是 `/health` 返回 200、读接口全部正常，但一写数据就失败：

  ```
  [account] save accounts failed: open /app/data/accounts.json.tmp: permission denied
  ```

  三个 compose 都加了 `init-data` 一次性容器，`app` 通过
  `depends_on: service_completed_successfully` 等它把 `./data` chown 到 65532。
+ [修复] **`docker-compose.local.yml` 无限重启**：它默认用 sqlite 后端，需要
  **创建** `data/accounts.db`，在权限问题下直接
  `storage: unable to open database file (14)` 并反复重启。修完权限后正常。
  （json 后端因为空号池时延迟写入，反而「看起来能跑」，掩盖了同一个根因。）

### 新增

+ [新增] `chatgpt2api --init-proxy-config` 子命令：从 `CHATGPT2API_PROXY_RUNTIME_*`
  环境变量推导 WARP/FlareSolverr 的 `proxy_runtime` 默认值，幂等合并进
  `config.json` 后退出。**不启动服务、不需要 `auth-key`** —— 正是为了能在
  用户填 key 之前把配置写好。幂等语义：已配置过的只补齐缺失键，不覆盖手改值；
  `CHATGPT2API_PROXY_RUNTIME_FORCE=true` 强制覆盖。输出一行脱敏摘要
  （URL 里的密码变 `[REDACTED]`）。13 个测试 / 37 个子测试覆盖。
  设计取舍见 [ADR-0013](./docs/adr/0013-warp-init-config-as-go-subcommand.md)。

### 文档

+ [文档] README / README.en / `docs/deployment.md` 补上 `--init-proxy-config`
  的说明与手动用法（此前三份文档零提及，而它是 WARP 部署的必需组件）。
+ [文档] `.env.example` 补 `CHATGPT2API_CONFIG_FILE` 与
  `CHATGPT2API_PROXY_RUNTIME_FORCE`，并说明这组变量由子命令消费、主程序不读。
+ [文档] README 的部署对比表补上 `docker-compose.local.yml`（它一直存在且可用，
  但此前三份文档都没提）。写明与默认 compose 的三个差异：本地 `build:` vs
  拉 GHCR、默认端口 8000 vs 3000、默认存储 sqlite vs json。
+ [文档] `docs/deployment.md` 的排障章节补充：`init-data` 做什么、为什么必须
  这么做、手动处理方式、如何验证属主正确。
+ [文档] 明确标注**未验证**的部分：完整 WARP 链路需要真实 WARP 容器与
  `NET_ADMIN`，当前环境无法端到端跑通；postgres / git 存储后端需要外部服务。
+ [文档] 新增 [ADR-0013](./docs/adr/0013-warp-init-config-as-go-subcommand.md)，
  记录「为什么用 Go 子命令而不是恢复 Python 脚本」。

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
