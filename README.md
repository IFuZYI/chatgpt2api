# ChatGPT2API

<p align="center">
  <b>Go 单二进制 · ChatGPT 号池代理 · OpenAI / Anthropic 兼容 API</b>
</p>

<p align="center">
  <a href="./README.en.md">English</a> ·
  <a href="./docs/deployment.md">部署文档</a> ·
  <a href="./docs/adr/">架构决策</a> ·
  <a href="./CHANGELOG.md">更新日志</a>
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white">
  <img alt="Next.js" src="https://img.shields.io/badge/Next.js-16-000000?logo=nextdotjs&logoColor=white">
  <img alt="License" src="https://img.shields.io/badge/license-MIT-blue">
  <img alt="Tests" src="https://img.shields.io/badge/tests-576%20passing-brightgreen">
</p>

---

把 ChatGPT 网页端的能力封装成标准 API，并管理一个多账号池。

后端是 **Go 1.27 + Gin 编译出的单个静态二进制**，前端用 Next.js 静态导出后
`go:embed` 进同一个二进制 —— 部署时只有**一个文件**，没有运行时依赖、没有
`node_modules`、没有 Python 环境。数据全部落在本地，四种存储后端可选。

> [!WARNING]
> **免责声明**
>
> 本项目通过对 ChatGPT 网页端接口的逆向研究实现，仅供个人学习、技术研究与
> 非商业性技术交流使用。
>
> - 严禁用于商业用途、盈利性使用、批量操作、自动化滥用或规模化调用。
> - 严禁用于破坏市场秩序、恶意竞争、套利倒卖、二次售卖相关服务，或任何违反
>   OpenAI 服务条款及当地法律法规的行为。
> - 严禁用于生成、传播或协助生成违法、暴力、色情、未成年人相关内容，或用于
>   诈骗、欺诈、骚扰等非法用途。
> - 使用者自行承担全部风险，包括账号被限流、临时封禁、永久封禁，以及因违规
>   使用导致的法律责任。
> - 使用本项目即视为已充分理解并同意本免责声明。请勿使用重要账号、常用账号或
>   高价值账号进行测试。

---

## 目录

- [它是什么](#它是什么)
- [快速开始](#快速开始)
- [配置](#配置)
- [功能](#功能)
- [API](#api)
- [开发](#开发)
- [架构](#架构)
- [已知限制](#已知限制)
- [文档](#文档)

---

## 它是什么

**一句话**：一个自己托管的 ChatGPT 转 API 网关 + 账号池管理器。

具体提供三类能力：

| 能力 | 说明 |
|:--|:--|
| **协议转换** | 把 ChatGPT 网页端的会话/生图链路包装成 OpenAI 与 Anthropic 兼容的 HTTP API |
| **号池调度** | 多账号轮询、槽位并发控制、失效自动剔除、限流定时刷新、密码重登恢复 |
| **Web 面板** | 在线画图、号池管理、图片管理、日志、设置 —— 全部内置在同一二进制里 |

**技术形态**：

```
┌─────────────────────────────────────────────┐
│  单个静态二进制 (CGO_ENABLED=0)              │
│  ├── Gin HTTP 服务                          │
│  ├── Next.js 静态产物（go:embed）            │
│  └── 8 个内部包，依赖严格单向                 │
└─────────────────────────────────────────────┘
              │                    │
        ┌─────┴─────┐        ┌─────┴──────┐
        │ 本地状态   │        │ 上游出口    │
        │ json/sqlite│        │ 直连/代理   │
        │ pg/git     │        │ WARP/CF    │
        └───────────┘        └────────────┘
```

**规模**：31,350 行非测试 Go 代码 + 20,079 行测试，576 个测试全绿。

---

## 快速开始

### Docker（推荐）

```bash
git clone https://github.com/IFuZYI/chatgpt2api.git
cd chatgpt2api

# 1. 生成 config.json。compose 用 create_host_path: false 绑定它 ——
#    文件不存在时 compose 直接报错。这是刻意的：短语法会静默建一个
#    同名目录，服务读不到配置，而报错信息是「config 是目录」，很难排查。
cp config.example.json config.json

# 2. 设置认证密钥。用 openssl rand -hex 32 生成，
#    不要用项目名或 admin 之类的可猜值。
$EDITOR config.json     # 填 "auth-key"

# 3. 启动
docker compose up -d
```

> **不想用 config.json？** 只设环境变量也能跑 —— 见
> [配置](#配置) 一节的说明。但 compose 的文件挂载仍需要该文件存在，
> 所以走 Docker 时轻量做法是 `cp config.example.json config.json`
> 后**只用环境变量覆盖**，不必编辑文件。

| | |
|:--|:--|
| Web 面板 | `http://localhost:3000` |
| API 地址 | `http://localhost:3000/v1` |
| 数据目录 | `./data` |

> 容器内固定监听 **8080**：distroless 的 `nonroot`（uid 65532）没有
> `CAP_NET_BIND_SERVICE`，绑不上 <1024 的端口。对外端口由 `CHATGPT2API_PORT`
> 决定，默认 `3000`。

> `data/` 的属主由 compose 内置的 `init-data` 一次性容器自动改成 uid 65532。
> 不这么做的话服务会「`/health` 200 但一写数据就 `permission denied`」——
> 见[排障](./docs/deployment.md#容器起来了但写数据报-permission-denied)。

更新：

```bash
docker compose pull && docker compose up -d
```

### 直接下载二进制

从 [Releases](https://github.com/IFuZYI/chatgpt2api/releases) 下载：

| 平台 | 文件 |
|:--|:--|
| Linux x86_64 | `chatgpt2api_*_linux_amd64` |
| Linux ARM64 | `chatgpt2api_*_linux_arm64` |
| Linux ARMv7 | `chatgpt2api_*_linux_armv7` |
| macOS Intel / Apple Silicon | `chatgpt2api_*_darwin_amd64` / `_darwin_arm64` |
| Windows x64 | `chatgpt2api_*_windows_amd64.exe` |

```bash
sha256sum -c SHA256SUMS.txt          # 校验
chmod +x chatgpt2api_*_linux_amd64

mkdir -p /opt/chatgpt2api && cd /opt/chatgpt2api
CHATGPT2API_AUTH_KEY=$(openssl rand -hex 32) \
  /path/to/chatgpt2api_*_linux_amd64 --addr :8080
```

首次运行会在当前目录创建 `data/`。二进制**已内置前端**，不需要额外部署静态文件。

`config.json` **不会**被自动创建 —— 缺少它时服务用空配置 + 环境变量启动。
若要改配置项（如图片保留天数、代理、备份），把模板复制过来再改：

```bash
curl -fsSLO https://raw.githubusercontent.com/IFuZYI/chatgpt2api/main/config.example.json
mv config.example.json config.json && $EDITOR config.json
```

服务正常启动后，其余配置项也可以直接在 Web 面板里改（会写回 `config.json`）。

### 源码运行

需要 **Go 1.27+** 与 **Node 22+**。

```bash
git clone https://github.com/IFuZYI/chatgpt2api.git
cd chatgpt2api

# web_dist 里有 .gitkeep 占位，不构建前端也能编译。
go build -o chatgpt2api ./cmd/server
CHATGPT2API_AUTH_KEY=dev-secret ./chatgpt2api --addr :8000
```

> 源码运行请用 **`:8000`**：前端 dev server 硬编码指向 `http://127.0.0.1:8000`
> （见 `web/src/constants/common-env.ts`）。Docker 那套（容器 8080 / 对外 3000）
> 是另一套端口，两者互不影响。

前端热重载：

```bash
cd web && npm install && npm run dev
```

让后端直接用未 embed 的前端产物：

```bash
cd web && npm run build                # 产物在 web/out
CHATGPT2API_WEB_DIST=$PWD/web/out CHATGPT2API_AUTH_KEY=dev ./chatgpt2api --addr :8000
```

### WARP / FlareSolverr（可选）

图片链路经常遇到 Cloudflare 拦截时：

```bash
cp .env.example .env
docker compose -f docker-compose.warp.yml up -d --build
```

会额外启动 `warp`（出口）、`privoxy`（SOCKS5→HTTP）、`flaresolverr`（挑战处理）
与一次性的 `init-config`（写 proxy_runtime 默认值）。

> ⚠️ `warp` 需要 `NET_ADMIN` / `SYS_MODULE`；`init-config` 会挂载项目目录写
> `config.json`。请确认宿主机目录权限，并注意 `.env` 里的凭据对其他容器可见。

### 四种部署方式对比

| 方式 | 文件 | 适用场景 | 前端 | 数据 | 升级 |
|:--|:--|:--|:--|:--|:--|
| Docker（拉镜像） | `docker-compose.yml` | 生产自托管 | 镜像内 embed | `./data` 卷 | `docker compose pull` |
| Docker（本地构建） | `docker-compose.local.yml` | 改了代码想直接跑 | 构建期 embed | `./data` 卷 | `docker compose up -d --build` |
| WARP 代理 | `docker-compose.warp.yml` | 上游被 Cloudflare 拦 | 构建期 embed | `./data` 卷 | `docker compose -f ... up -d --build` |
| 单二进制 | Releases 下载 | 无 Docker 环境、边缘设备 | 编译期 embed | 当前目录 `data/` | 替换文件后重启 |
| 源码运行 | `go build` | 开发调试 | `CHATGPT2API_WEB_DIST` | 当前目录 `data/` | 重新编译 |

`docker-compose.local.yml` 与默认 compose 的区别：它用 `build:` 从本地源码构建
（而不是拉 GHCR 镜像），默认端口 `8000`（不是 3000），默认存储后端 `sqlite`
（不是 json）。适合「clone 下来改了代码想立刻验证」的场景：

```bash
cp config.example.json config.json && $EDITOR config.json
docker compose -f docker-compose.local.yml up -d --build
```

---

## 配置

**部署时只改 `config.json`** —— 它是唯一的运行时配置文件。

| 文件 | 角色 | 应用读它吗 |
|:--|:--|:--|
| `config.example.json` → `config.json` | 真正的配置 | ✅ |
| `.env.example` → `.env` | 只给 compose 做变量替换（端口等） | ❌ |
| `docker-compose.yml` | 部署编排 | ❌（Docker 读） |

配置来源与优先级：**环境变量 > `config.json` > 代码默认值**。
但只有少数几类键支持环境变量，详见
[配置说明](./docs/configuration.md)（含实测的优先级证据与常见误解）。

但要注意**范围**：只有下面这些项支持环境变量，其余项（图片保留天数、
代理、备份、审核等）只能写 `config.json` 或用 Web 面板改。

| 只能通过环境变量或 `config.json` | 说明 |
|:--|:--|
| `CHATGPT2API_AUTH_KEY` | 认证密钥。**没有默认值**，两处都没设则服务拒绝启动 |
| `CHATGPT2API_BASE_URL` | 对外基础 URL |
| `CHATGPT2API_BASE_DIR` | 数据根目录 |
| `CHATGPT2API_ADDR` | 监听地址 |
| `CHATGPT2API_DEBUG` | 诊断日志开关 |
| `CHATGPT2API_WEB_DIST` | 前端静态目录 |
| `STORAGE_BACKEND` / `DATABASE_URL` | 存储后端 |
| `GIT_REPO_URL` / `GIT_TOKEN` / `GIT_BRANCH` / `GIT_FILE_PATH` / `GIT_AUTH_KEYS_FILE_PATH` | git 后端 |

`config.json` 是**可选**的：不存在时服务用空配置启动（`readJSONObject`
返回空 map，不报错）。所以**纯靠环境变量的最小部署是可行的**：

```bash
CHATGPT2API_AUTH_KEY=$(openssl rand -hex 32) ./chatgpt2api --addr :8080
```

只要 `auth-key` 有值（来自环境变量或文件）就能起来。

### 环境变量

| 变量 | 说明 | 默认 |
|:--|:--|:--|
| `CHATGPT2API_AUTH_KEY` | 认证密钥。**必填**，留空拒绝启动 | 无 |
| `CHATGPT2API_BASE_URL` | 对外基础 URL（生成图片链接用） | 空，回退到请求 `Host` |
| `CHATGPT2API_ADDR` | 监听地址 | `:8080`（compose 内） |
| `CHATGPT2API_BASE_DIR` | 数据根目录 | 二进制所在目录 |
| `CHATGPT2API_DEBUG` | 置 `1` 输出 `[image]` 诊断日志 | 关 |
| `CHATGPT2API_WEB_DIST` | 前端静态目录（开发用，覆盖 embed） | 空 |
| `CHATGPT2API_MAX_IMAGE_PIXELS` | 单张图片解码像素上限（防 OOM） | `8000000` |
| `STORAGE_BACKEND` | `json` / `sqlite` / `postgres` / `git` | `json` |
| `DATABASE_URL` | sqlite 用文件路径，postgres 用连接串 | 空 |

完整清单（含 git 后端、代理运行时）见 [`.env.example`](./.env.example)。

### 命令行参数

| 参数 | 说明 |
|:--|:--|
| `--addr` | 监听地址，如 `:8080` |
| `--base-dir` | 数据根目录 |
| `--healthcheck` | 探活后退出（容器 healthcheck 用；distroless 无 curl） |
| `--init-proxy-config` | 把 WARP/FlareSolverr 的 `proxy_runtime` 默认值幂等合并进 `config.json` 后退出。`docker-compose.warp.yml` 的 `init-config` 容器用它 |

> `--init-proxy-config` 读一组 `CHATGPT2API_PROXY_RUNTIME_*` 环境变量
> （见 [`.env.example`](./.env.example)），合并完打印一行脱敏摘要。
> 它**不启动服务**，也不需要 `auth-key` —— 正因为要能在「用户还没填 key」时
> 把配置写好。手动用法：
>
> ```bash
> CHATGPT2API_CONFIG_FILE=./config.json ./chatgpt2api --init-proxy-config
> ```
>
> 幂等：已配置过的 `proxy_runtime` 只补齐缺失键，不会覆盖你手改的值；
> 想强制覆盖设 `CHATGPT2API_PROXY_RUNTIME_FORCE=true`。

### 存储后端

| 后端 | 说明 | 何时选它 |
|:--|:--|:--|
| `json` | 本地 JSON 文件（默认） | 账号少、单实例 |
| `sqlite` | 本地 SQLite（CGO-free） | 账号较多；省内存 |
| `postgres` | 外部 PostgreSQL | 需外部备份或独立数据库 |
| `git` | Git 私有仓库 | 想用 git 做版本化备份 |

```yaml
# PostgreSQL 示例
environment:
  STORAGE_BACKEND: postgres
  DATABASE_URL: postgresql://user:password@host:5432/dbname
```

```yaml
# SQLite 示例 —— 注意是 4 个斜杠
# sqlite:/// 会被当成相对路径，随后报 "unable to open database file"，
# 很难联想到是斜杠数量问题。
environment:
  STORAGE_BACKEND: sqlite
  DATABASE_URL: sqlite:////app/data/accounts.db
```

> **单实例约束**：本地状态 + 进程内任务队列，**不支持多实例水平扩展**。
> 见 [ADR-0004](./docs/adr/0004-single-instance-local-state.md)。

---

## 功能

### API 兼容

| 接口 | 状态 |
|:--|:--|
| `POST /v1/images/generations` | ✅ OpenAI 兼容，`n=1–4`，支持 `stream` |
| `POST /v1/images/edits` | ✅ multipart 与 JSON（data URL） |
| `POST /v1/chat/completions` | ✅ 文本 / 图片 / 网页搜索三态，SSE 流式 |
| `POST /v1/responses` | ✅ 含 `image_generation` / `web_search` 工具 |
| `POST /v1/messages` | ✅ Anthropic 兼容（独立错误信封） |
| `POST /v1/search` | ✅ 网页搜索 |
| `POST /v1/ppt/generations` | ✅ PPT 导出（异步任务） |
| `POST /v1/psd/generations` | ✅ PSD 导出（异步任务） |
| `GET /v1/models` | ✅ 文本模型动态拉取 + 图片模型内置 |
| `POST /v1/images/edits` 的 `http(s)://` 引用 | ❌ 返回 400（不代拉远程图片） |

### 模型

`GET /v1/models` 返回 15 个模型，随号池能力变化：

- **文本**（11）：`auto`、`gpt-5-3-mini`、`gpt-5-6`、`gpt-5-6-mini`、
  `gpt-5-6-t-mini`、`gpt-5-6-t-mini-mini`、`gpt-6`、`gpt-6-mini`、
  `gpt-6-t-mini`、`gpt-6-t-mini-mini`、`research`
- **图片**（4）：`gpt-image-2`、`gpt-image-2.5`、`gpt-image-2.5-flare`、
  `gpt-image-2.5-sunburst`

文本模型从上游 `/backend-api/models` 运行时拉取（TTL 300s，账号变更即失效），
新增文本模型不需要改代码。图片模型是内置表。

> **Codex 画图**（`codex-gpt-image-2`）走 `/backend-api/codex/responses`，
> 仅 `Plus` / `Team` / `Pro` 且 `source_type=codex` 的账号可用。
> 请求侧已逐字对齐，**响应事件格式未经真实上游验证**；形态不符时明确报
> `No image result found in response`，不会静默返回空图。
> 见 [ADR-0010](./docs/adr/0010-codex-responses-response-shape-unverified.md)。

### 在线画图

- 生成、单图编辑、多图组图编辑
- 参考图上传、多图生成交互
- 本地会话历史（回看 / 删除 / 清空）
- 服务端图片 URL 缓存、进度追踪、超时后可继续等待
- 图片懒加载与滚动位置记忆

### 号池管理

- 自动刷新邮箱、类型、额度、恢复时间（异步进度追踪）
- 按槽位轮询账号执行生图 / 编辑（每账号并发可配，默认 3）
- Token 失效（401/403）自动剔除；**429 限流不误删**
- 定时检查限流账号并刷新
- **密码重新登录**（含 TOTP 两步验证）恢复异常账号
- **`refresh_token` 换 `access_token`** keepalive，避免长期闲置导致 rt 失效
- **`auto_relogin_after_refresh`**：刷新发现失效账号后自动重登（429 不触发）
- 全局 HTTP / HTTPS / SOCKS5 / SOCKS5H 代理
- WARP / FlareSolverr 代理运行时
- 搜索、筛选、批量刷新、导出、手动编辑、清理
- 四种导入：本地 CPA JSON、远程 CPA 服务器、`sub2api` 服务器、`access_token`
- 图片存储三模式：`local` / `webdav` / `both`

### 图片存储

| 模式 | 行为 | 适用 |
|:--|:--|:--|
| `local`（默认） | 只写本地磁盘 | 单机、图不多 |
| `webdav` | 只写 WebDAV；失败即整次失败 | 想彻底不占本地盘 |
| `both` | 本地 + WebDAV | 兼顾可靠性 |

`both` 模式下 WebDAV 失败会**降级为本地并返回成功**，索引如实标记
`storage=local` / `webdav=false`，远端上传异步进行不阻塞生图。
`webdav` 模式则相反 —— 失败即失败，避免用户以为图已在远端而本地磁盘照样涨满。
见 [ADR-0011](./docs/adr/0011-both-mode-degrades-on-webdav-failure.md) /
[ADR-0012](./docs/adr/0012-both-mode-async-upload.md)。

---

## 安全

### 请求体上限

入站请求体有硬上限，超限返回 `413`：

| 端点类别 | 上限 |
|:--|--:|
| 控制面 JSON（设置、账号、日志等） | 8 MiB |
| base64 图片端点（`/v1/images/*`、`/v1/chat/completions`、`/v1/responses`） | 16 MiB |

控制面取 8 MiB 而不是更小：`POST /api/accounts` 导入的是整个号池的 JSON，
实测 260 个账号的导出文件已有 880 KB（约 3.5 KB/账号）—— 1 MiB 只能装
302 个账号，一个 300 号以上的池子导出后**无法再导入**。8 MiB 可容纳约 2400 个。

没有上限时，`/v1/images/edits` 的 mask 合成会在**号池配额与并发槽位准入之前**
解码图片 —— 实测 1.5 MB 请求体消耗 459 MB 内存与 52 秒 CPU，几个并发即可 OOM。

### 图片解码像素预算

单张图片解码后的像素数上限默认 **800 万**（约 2828×2828）。

为什么不是对齐 Pillow：Pillow 的硬错误阈值是 1.79 亿像素，但那是**库**的阈值，
不是**进程能承受**的预算。实测分配系数是 **14.2 字节/像素**（不是单份 NRGBA
的 4 字节）—— 合成路径同时持有原始图、合成结果、缩放后的 mask、mask 灰度图
与 alpha 通道五份缓冲。800 万像素实测峰值 RSS 约 **144 MB**。

同时 `mask` 合成有**并发闸门**（默认 2 个）：它发生在号池配额与并发槽位
准入之前，不设闸门时 N 个并发就是 N × 144 MB。实测 4 个并发峰值仅 +168 MB。

大内存部署可放宽：

```bash
CHATGPT2API_MAX_IMAGE_PIXELS=30000000 ./chatgpt2api --addr :8080
```

### 与 OpenAI 官方限额的差异

上游 `/v1/images/edits` 的请求体上限约 50 MB，本服务为 16 MiB。
超过 16 MiB 的 base64 上传会被 413 —— 这是有意的取舍：单实例内存有限，
且解码后的像素预算（800 万）通常先成为实际约束。

### 部署建议

+ **必须设 `CHATGPT2API_AUTH_KEY`** —— 留空拒绝启动。用 `openssl rand -hex 32` 生成。
+ **不要把 `data/` 暴露到公网**。`accounts.json` 含号池全部凭据，`config.json`
  含出口代理密码。容器内以 uid 65532 运行，建议宿主目录也只给该 uid 可读。
+ **轮换曾泄漏的代理凭据**。`config.example.json` 在 `1656ba3` 曾携带真实 SOCKS5
  凭据，虽已在 `e3be6e5` 清空并推送，但**该值已进入 git 历史** —— 修复 HEAD
  不等于修复泄漏，任何 clone 过的人手里都有一份。唯一可靠的处置是轮换密码。
+ **备份默认不加密**。`include` 里勾选的凭据会随归档上传到配置的对象存储，
  确认目标存储可信。

## API

所有 `/v1/*` 与 `/api/*` 都需要：

```http
Authorization: Bearer <auth-key>
```

三套错误信封（对应三种客户端生态，见 [ADR-0005](./docs/adr/0005-three-error-envelopes.md)）：

| 接口组 | 错误形状 |
|:--|:--|
| `/api/*`（管理面） | `{"detail": ...}` |
| OpenAI 兼容 | `{"error": {"message","type","param","code"}}` |
| Anthropic | `{"type":"error","error":{"type","message"}}` |

### 图片生成

```bash
curl http://localhost:3000/v1/images/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth-key>" \
  -d '{
    "model": "gpt-image-2",
    "prompt": "一只漂浮在太空里的猫",
    "n": 1,
    "response_format": "b64_json"
  }'
```

| 字段 | 说明 |
|:--|:--|
| `model` | 以 `/v1/models` 为准，推荐 `gpt-image-2` |
| `n` | `1`–`4`。**越界返回 422，不静默夹取** |
| `response_format` | `b64_json`（默认）或 `url` |
| `stream` | `true` 走 SSE；失败会在流内补 `error` 事件再 `[DONE]` |

### 图片编辑

```bash
curl http://localhost:3000/v1/images/edits \
  -H "Authorization: Bearer <auth-key>" \
  -F "model=gpt-image-2" \
  -F "prompt=把背景换成海边" \
  -F "image=@input.png"
```

`n` 越界返回 400（对齐上游语义）。`http(s)://` 引用返回 400 —— 不代拉远程图片。

### 异步任务（PPT / PSD）

```bash
# 建任务
curl -X POST http://localhost:3000/v1/ppt/generations \
  -H "Authorization: Bearer <auth-key>" \
  -H "Content-Type: application/json" \
  -d '{"prompt": "季度复盘", "client_task_id": "my-task-1"}'

# 查任务
curl "http://localhost:3000/v1/editable-file-tasks?ids=my-task-1" \
  -H "Authorization: Bearer <auth-key>"
```

产物在 `data/files/ppt/<task_id>/`，通过 `/files/ppt/<task_id>/<文件名>` 访问。
`client_task_id` 省略时服务端生成 UUID；它是**幂等键**，同一 owner 重复提交同一 id 会复用任务。

### 管理面

共 **77 条路由**（GET 30 / POST 42 / DELETE 5）。完整清单以
[`internal/api/`](./internal/api/) 下的 `*_routes.go` 为准。常用几组：

| 组 | 端点前缀 |
|:--|:--|
| 号池 | `/api/accounts`（增删改查、刷新、重登、导入、导出） |
| 图片 | `/api/images`（列表、标签、删除、下载、压缩、按目标清理） |
| 图片任务 | `/api/image-tasks`（提交、查询、resume-poll） |
| 设置 | `/api/settings`、`/api/proxy/*`、`/api/image-storage/*` |
| 备份 | `/api/backups`（R2 上传 / 列举 / 下载） |
| 集成 | `/api/cpa/pools/*`、`/api/sub2api/servers/*` |
| 用户密钥 | `/api/auth/users`（仅 admin） |
| 日志 | `/api/logs`（仅 admin） |

---

## 开发

### 测试

```bash
go test ./...                                 # 576 个
go test -race ./...                           # 竞态检测
go test -tags live ./internal/upstream/ -v    # 真实上游握手（需外网）
```

`-tags live` 的用例只记录上游状态、不设失败断言 —— 上游变了应该被观察到，
而不是让 CI 变红。

覆盖率（语句）：

| 包 | 覆盖率 | 包 | 覆盖率 |
|:--|--:|:--|--:|
| `web` | 91.5% | `config` | 65.1% |
| `health` | 88.5% | `upstream` | 55.8% |
| `protocol` | 78.2% | `service` | 56.1% |
| `api` | 44.1% | `storage` | 44.1% |

PostgreSQL 后端测试需要 `TEST_POSTGRES_URL`，未设置时跳过。

### 发布

打 tag 触发两条独立流水线：

```bash
git tag v2.0.2 && git push origin v2.0.2
```

| 工作流 | 作用 |
|:--|:--|
| [`release.yml`](./.github/workflows/release.yml) | 跨平台编译二进制、生成 `SHA256SUMS.txt`、建 GitHub Release |
| [`docker-publish.yml`](./.github/workflows/docker-publish.yml) | 构建 amd64/arm64 镜像推到 GHCR |

`release.yml` 也支持手动触发（`workflow_dispatch`），只跑构建不建 Release。

---

## 架构

8 个包，依赖**严格单向**（`go list -deps` 验证过，含测试文件）：

```
protocol   storage   web   config      ← 无内部依赖
      ↑
   upstream  →  protocol
      ↑
    service  →  config, protocol, storage, upstream
      ↑
      api    →  config, protocol, service, storage, web
```

**为什么这样切**：`service` 不 import `upstream` 也不 import `api` ——
上游访问通过函数类型的接缝注入（`ImageUpstreamFactory`、`TaskRunner`、
`EditableFileTaskRunner`）。这样业务逻辑可以完全用假上游测试，
也让「换上游实现」不会牵动业务层。

**错误信封**集中在一处（`internal/api/errors.go`），不在各 handler 里散落。

关键设计决策记录在 [`docs/adr/`](./docs/adr/) —— 13 条，包括：
为什么全量重写、TLS 指纹怎么复刻、三套错误信封的边界、数据格式为何逐字保留、
goroutine 槽位模型、单实例约束、`both` 模式为何降级、为何异步上传。

---

## 已知限制

诚实列出，不假装完整：

| 限制 | 说明 |
|:--|:--|
| 单实例 | 本地状态 + 进程内队列，不支持水平扩展 |
| 后台任务不可取消 | 生图任务一旦开跑无法中断，`Close` 只能等满超时 |
| 流式非增量 | 图片与文本都是跑完再一次性写出 chunk；长请求期间客户端收不到字节，反代需关缓冲 |
| 进度对象不淘汰 | 每次 admin 刷新/重登保留一份号池副本，重启才回收 |
| 可编辑任务无保留策略 | `data/files/` 与任务记录无自动清理 |
| 入站请求体无上限 | 大请求体可推高进程内存 |
| 备份默认不加密 | 归档内含号池全部凭据，需显式开启 `backup.encrypt` |
| Codex 响应格式未验证 | 请求侧已对齐，响应事件形态未经真实上游确认（ADR-0010） |
| 匿名对话不可用 | 上游限制，非实现问题（ADR-0008） |

完整清单与成因见 [ADR-0009](./docs/adr/0009-go-rewrite-scope-and-gaps.md)。

---

## 文档

| 文档 | 内容 |
|:--|:--|
| [配置说明](./docs/configuration.md) | **config.json / .env / compose 三者关系与优先级** —— 部署时该改哪个 |
| [图片存储三模式](./docs/image-storage-modes.md) | `local` / `webdav` / `both` 的写入、读取、删除全流程与实测耗时 |
| [部署文档](./docs/deployment.md) | 部署形态、反向代理、存储后端、备份、升级回滚、安全加固、排障 |
| [功能清单](./docs/feature-status.en.md) | 逐项功能状态 |
| [ADR](./docs/adr/) | 13 条架构决策记录 |
| [上游 SSE 协议](./docs/upstream-sse-conversation.md) | 会话流式协议记录 |
| [Python 后端归档](./docs/archive-python-backend.md) | 1.x 的 Python 实现在哪个分支、为什么移除 |
| [术语表](./GLOSSARY.md) | 项目内术语 |
| [更新日志](./CHANGELOG.md) | 版本变更 |

---

## 社区

学 AI，上 L 站：[LinuxDO](https://linux.do)

## 许可

[MIT](./LICENSE)

## Star History

[![Star History Chart](https://api.star-history.com/chart?repos=IFuZYI/chatgpt2api&type=date&legend=top-left)](https://www.star-history.com/?repos=IFuZYI%2Fchatgpt2api&type=date&legend=top-left)
