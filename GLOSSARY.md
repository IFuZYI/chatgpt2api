# ChatGPT2API

把 ChatGPT 官网的对话/画图/搜索能力逆向封装成 OpenAI 兼容 API，并附带号池管理、在线画图工作台和自托管部署能力。本文件只定义这个项目自己的语言，不解释通用编程概念。

## 语言

**上游 (Upstream)**：
本项目所逆向的 ChatGPT 官网服务端，域名固定为 `chatgpt.com` 及其 sentinel 子域。
_Avoid_: 远端、官方、OpenAI 服务

**号池 (Account Pool)**：
一组可轮换使用的 ChatGPT 账号，是本项目唯一的出站身份来源。号池是单写者状态：同一份 `data/accounts.json` 只允许一个进程写。
_Avoid_: 账号列表、账号库、账号集合

**账号槽位 (Account Slot)**：
某个账号上同时进行的图片生成数量配额，上限由 `image_account_concurrency` 决定。槽位必须成对获取与释放，泄漏会表现为号池"空闲但无号可用"。
_Avoid_: 并发数、连接数、限流

**在途图片 (Image Inflight)**：
已占用账号槽位、尚未产出结果的图片请求。对外暴露为账号的 `image_inflight` 字段，用于诊断槽位泄漏。
_Avoid_: 运行中任务、pending

**限流账号 (Limited Account)**：
额度耗尽、进入冷却期并带有 `restore_at` 恢复时间的账号。它不是异常账号；文本链路仍可使用，图片链路等待额度恢复。
_Avoid_: 封号、不可用

**异常账号 (Abnormal Account)**：
Access Token 被判定失效、已暂停调度，但自动路径仍会尝试恢复的账号。失败按指数退避（带抖动）重试；恢复成功回到正常，不可恢复则转为禁用。
_Avoid_: 封号、禁用账号

**禁用账号 (Disabled Account)**：
自动路径已判定无法恢复的终态账号。封号、凭据失效、缺密码或缺 2FA 密钥都可能触发禁用；禁用后不再自动刷新、续期或重登，人工「尝试恢复」是唯一解禁通道。
_Avoid_: 异常账号、封号（封号只是成因之一）

**重登 (Re-login)**：
用账号保存的邮箱、密码与可选 TOTP 重新完成上游授权，换取新的 Access Token、Refresh Token 与 ID Token。它不同于刷新账号信息（后者只读取邮箱、套餐与额度，不换 token）。
_Avoid_: 刷新、移除异常状态

**账号出口代理 (Account Egress Proxy)**：
绑定到单个账号、用于该账号所有上游请求的固定出口。优先级为：账号代理 → 全局代理 → 直连；密码重登、RT 续期也必须遵守，避免同一账号因路径不同切换 IP。
_Avoid_: 随机代理、代理池

**稳定代理运行时 (Proxy Runtime)**：
设置页里用于 WARP / FlareSolverr / Cloudflare 清障的运行时配置。它目前负责状态展示与代理测试；真实账号出站由**账号出口代理**或全局 `proxy` 决定。
_Avoid_: 账号级代理、全局代理

**清障 (Clearance)**：
为绕过 Cloudflare 拦截而获取并复用 `cf_clearance` cookie 的过程，来源可以是手动粘贴、FlareSolverr 自动求解，或直接关闭。
_Avoid_: 验证码、过盾、解盾

**指纹 (Fingerprint)**：
出站请求的浏览器身份，由 TLS ClientHello 特征、header 顺序、`User-Agent`、`sec-ch-ua*` 与 `oai-device-id` 共同构成。上游同时校验其中多项，只改 User-Agent 不构成有效指纹。
_Avoid_: UA、请求头、伪装

**协议信封 (Error Envelope)**：
错误响应的外层形状。本项目有三套互不兼容的信封：管理面用 `{"detail": ...}`，`/v1/*` 用 `{"error": {"message", "type", "param", "code"}}`，Anthropic 的 `/v1/messages` 用 `{"type": "error", "error": {"type", "message"}}`。
_Avoid_: 错误格式、返回体

**图片任务 (Image Task)**：
带 `client_task_id` 的异步图片作业，状态机为 `queued → running → success | error`。它与**图片请求**（同步等待结果的 `/v1/images/*` 调用）是两条独立链路。
_Avoid_: 异步请求、后台任务

**可编辑文件任务 (Editable File Task)**：
让上游产出可编辑 PPT/PSD 的异步作业，产物落在 `data/files/`，通过 `/files/*` 下载。
_Avoid_: 文件生成、导出任务

**CPA / sub2api**：
两种外部账号来源。CPA 是远程 JSON 文件池，sub2api 是一台带账号管理 API 的服务器。两者都只负责把账号导入本地号池，导入后与本地账号无区别。
_Avoid_: 账号提供商、第三方平台

**Golden 基线 (Golden Baseline)**：
由 Python 版录制、用于证明 Go 版行为等价的响应快照集合，覆盖状态码、header 与 body 字节。它是重构的验收契约，不是回归测试的附属品。
_Avoid_: 快照测试、基准数据

**单二进制 (Single Binary)**：
后端以 `CGO_ENABLED=0` 编译的静态可执行文件，前端产物经 `go:embed` 一并打入。部署时只需要这一个文件加 `config.json` 与 `data/`。
_Avoid_: 可执行文件、程序、server

**上游适配器 (Upstream Adapter)**：
`internal/upstream` 里把 `*Client` 适配成 `protocol.Upstream` / `protocol.ImageUpstream` 接口的那一层。存在的理由是 protocol 需要调用上游、上游也需要 protocol 的类型，直接互相 import 会成环。
_Avoid_: 客户端封装、HTTP 层

**轮询预算 (Poll Budget)**：
一次图片生成允许花在「等上游把结果写进对话文档」上的总时长，由 `image_poll_timeout_secs` 决定。预算耗尽抛超时错误，前端据此显示「继续等待」。
_Avoid_: 超时时间、等待时长

**落定 (Settle)**：
找到图片 `file_id` 之后，再等一小段时间并复查一次，确认 id 集合不再增长。防止上游分批写入导致只拿到部分结果。
_Avoid_: 二次确认、延迟返回

**工具标记 (Tool Markup)**：
上游只会输出纯文本，Anthropic 兼容层让模型按约定输出 `<tool_calls>` XML，再由 `internal/protocol/anthropic.go` 解析成 `tool_use` 块。解析前要先剥离 ``` 代码块。
_Avoid_: 函数调用、tool schema
