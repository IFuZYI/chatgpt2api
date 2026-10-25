# Changelog

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
