<h1 align="center">ChatGPT2API</h1>


<p align="center">ChatGPT2API 主要是对 ChatGPT 官网相关能力进行逆向整理与封装，提供面向 ChatGPT 图片生成、图片编辑、多图组图编辑场景的 OpenAI 兼容图片 API / 代理，并集成在线画图、号池管理、多种账号导入方式与 Docker 自托管部署能力。</p>

> [!WARNING]
> 免责声明：
>
> 本项目涉及对 ChatGPT 官网文本生成、图片生成与图片编辑等相关接口的逆向研究，仅供个人学习、技术研究与非商业性技术交流使用。
>
> - 严禁将本项目用于任何商业用途、盈利性使用、批量操作、自动化滥用或规模化调用。
> - 严禁将本项目用于破坏市场秩序、恶意竞争、套利倒卖、二次售卖相关服务，以及任何违反 OpenAI 服务条款或当地法律法规的行为。
> - 严禁将本项目用于生成、传播或协助生成违法、暴力、色情、未成年人相关内容，或用于诈骗、欺诈、骚扰等非法或不当用途。
> - 使用者应自行承担全部风险，包括但不限于账号被限制、临时封禁或永久封禁以及因违规使用等所导致的法律责任。
> - 使用本项目即视为你已充分理解并同意本免责声明全部内容；如因滥用、违规或违法使用造成任何后果，均由使用者自行承担。
> - 本项目基于对 ChatGPT 官网相关能力的逆向研究实现，存在账号受限、临时封禁或永久封禁的风险。请勿使用你自己的重要账号、常用账号或高价值账号进行测试。


## 赞助商

<table>
  <tr>
    <td width="190" align="center">
      <a href="https://www.atlascloud.ai/zh?utm_source=github&utm_medium=link&utm_campaign=chatgpt2api"><img src="assets/atlascloud.svg" width="163" alt="Atlas Cloud"></a>
    </td>
    <td>
      <a href="https://www.atlascloud.ai/zh?utm_source=github&utm_medium=link&utm_campaign=chatgpt2api">Atlas Cloud</a> is a full-modal AI inference platform that gives developers a single AI API to access video generation, image generation, and LLM APIs. Instead of managing multiple vendor integrations, you connect once and get unified access to 300+ curated models across all modalities. Check out <a href="https://www.atlascloud.ai/console/coding-plan">Atlas Cloud's new coding plan promotion</a> for more budget-friendly API access.
    </td>
  </tr>
</table>


## 快速开始

### Docker（推荐）

```bash
git clone https://github.com/basketikun/chatgpt2api.git
cd chatgpt2api

# 1. 必须先生成 config.json —— compose 用 create_host_path: false 绑定它，
#    文件不存在 compose 会直接报错（这是刻意的：短语法会静默建一个同名目录，
#    服务读不到配置，报错却是「config 是目录」，很难排查）。
cp config.example.json config.json

# 2. 填认证密钥。留空服务会拒绝启动并明确报错。
#    也可以改用环境变量 CHATGPT2API_AUTH_KEY 覆盖。
$EDITOR config.json     # 设置 "auth-key"

# 3. 启动
docker compose up -d
```

- Web 面板：`http://localhost:3000`
- API 地址：`http://localhost:3000/v1`
- 数据目录：`./data`

> 容器内固定监听 **8080**（distroless 的 nonroot uid 65532 无法绑定 <1024 的端口）。
> 对外端口用 `CHATGPT2API_PORT` 覆盖，默认 `3000`。

后续更新：

```bash
docker compose pull && docker compose up -d
```

### 直接下载二进制

从 [Releases](https://github.com/basketikun/chatgpt2api/releases) 下载对应平台的文件
（linux/darwin/windows × amd64/arm64，另附 `linux/armv7`）：

```bash
chmod +x chatgpt2api_*_linux_amd64
CHATGPT2API_AUTH_KEY=your-secret ./chatgpt2api_*_linux_amd64 --addr :8080
```

首次运行会在当前目录创建 `config.json` 与 `data/`。校验完整性：

```bash
sha256sum -c SHA256SUMS.txt
```

### WARP / FlareSolverr 稳定代理部署

图片链路经常遇到 Cloudflare 拦截时，可启用附带的 WARP + Privoxy + FlareSolverr 方案：

```bash
cp .env.example .env
docker compose -f docker-compose.warp.yml up -d --build
```

该 compose 会启动：

- `warp` — Cloudflare WARP 出口（以 `NET_ADMIN` / `SYS_MODULE` 运行，仅监听 `127.0.0.1`）
- `privoxy` — 把 WARP 的 SOCKS5 转成 HTTP 代理（仅 `127.0.0.1`）
- `flaresolverr` — 自动处理 Cloudflare 挑战（仅 `127.0.0.1`）
- `init-config` — 一次性容器，把 proxy_runtime 默认值写进 `config.json`

> ⚠️ `init-config` 容器会把整个项目目录只读挂载进来写 `config.json`。
> 生产环境请确认宿主机上该目录的权限，以及 `.env` 里的凭据不会被其他容器读到。

---

## 部署方式对比

| 方式 | 适用场景 | 前端 | 数据 | 升级 |
|:--|:--|:--|:--|:--|
| Docker Compose | 生产自托管 | 镜像内已 embed | `./data` 卷 | `docker compose pull` |
| 单二进制 | 无 Docker 环境、边缘设备 | 编译期 embed | 当前目录 `data/` | 替换文件后重启 |
| 源码运行 | 开发调试 | `CHATGPT2API_WEB_DIST` 指向 `web/out` | 当前目录 `data/` | 重新编译 |

---

## 配置

配置来源优先级：**环境变量 > `config.json` > 代码默认值**。

### 环境变量

| 变量 | 说明 | 默认 |
|:--|:--|:--|
| `CHATGPT2API_AUTH_KEY` | 认证密钥。**必填**，留空服务拒绝启动 | 无 |
| `CHATGPT2API_BASE_URL` | 对外基础 URL（生成图片链接用） | 空，回退到请求的 `Host` |
| `CHATGPT2API_ADDR` | 监听地址 | `:8080`（compose 内） |
| `CHATGPT2API_BASE_DIR` | 数据根目录 | 二进制所在目录 |
| `CHATGPT2API_DEBUG` | 置 `1` 在 stderr 打 `[image]` 诊断日志 | 关 |
| `CHATGPT2API_WEB_DIST` | 前端静态目录（开发用，覆盖编译期 embed） | 空 |
| `STORAGE_BACKEND` | 存储后端：`json` / `sqlite` / `postgres` / `git` | `json` |
| `DATABASE_URL` | `sqlite` 用文件路径，`postgres` 用连接串 | 空 |

完整变量清单（含 git 后端、WARP 代理运行时）见 [`.env.example`](./.env.example)。

### 命令行参数

| 参数 | 说明 |
|:--|:--|
| `--addr` | 监听地址，如 `:8080` |
| `--base-dir` | 数据根目录 |
| `--healthcheck` | 探活后退出（容器 healthcheck 用；distroless 没有 curl） |

### 存储后端

| 后端 | 说明 | 何时选它 |
|:--|:--|:--|
| `json` | 本地 JSON 文件（默认） | 账号数少、单实例 |
| `sqlite` | 本地 SQLite（CGO-free） | 账号较多；省内存 |
| `postgres` | 外部 PostgreSQL | 多实例/需外部备份 |
| `git` | Git 私有仓库 | 想用 git 做版本化备份 |

示例（PostgreSQL）：

```yaml
environment:
  STORAGE_BACKEND: postgres
  DATABASE_URL: postgresql://user:password@host:5432/dbname
```

> 单实例约束：本项目使用本地状态 + 进程内任务队列，**不支持多实例水平扩展**，
> 见 [ADR-0004](./docs/adr/0004-single-instance-local-state.md)。

---

## 功能

### API 兼容能力

| 接口 | 状态 |
|:--|:--|
| `POST /v1/images/generations` | ✅ OpenAI 兼容，支持 `n=1–4`、`stream` |
| `POST /v1/images/edits` | ✅ 支持 multipart 与 JSON（data URL） |
| `POST /v1/chat/completions` | ✅ 含图片分支与网页搜索分支、SSE 流式 |
| `POST /v1/responses` | ✅ 含 `image_generation` / `web_search` 工具 |
| `POST /v1/messages` | ✅ Anthropic 兼容（独立错误信封） |
| `POST /v1/search` | ✅ 网页搜索 |
| `POST /v1/ppt/generations` | ✅ PPT 导出（异步任务） |
| `POST /v1/psd/generations` | ✅ PSD 导出（异步任务） |
| `GET /v1/models` | ✅ 动态聚合文本模型 + 内置图片模型 |
| `POST /v1/images/edits` 的 `http(s)://` 引用 | ❌ 明确返回 400（不自动拉取远程图片） |

`GET /v1/models` 返回的模型随号池能力变化。当前实测：

- **文本**：`auto`、`gpt-5-3-mini`、`gpt-5-6`、`gpt-5-6-mini`、`gpt-5-6-t-mini`、
  `gpt-5-6-t-mini-mini`、`gpt-6`、`gpt-6-mini`、`gpt-6-t-mini`、`gpt-6-t-mini-mini`、`research`
- **图片**：`gpt-image-2`、`gpt-image-2.5`、`gpt-image-2.5-flare`、`gpt-image-2.5-sunburst`

> Codex 画图链路（`codex-gpt-image-2`）走 `/backend-api/codex/responses`，仅
> `Plus` / `Team` / `Pro` 且 `source_type=codex` 的账号可用。请求侧已对齐 Python，
> **响应事件格式未对真实上游验证**；形态不符时会明确报
> `No image result found in response`，不会静默返回空图。
> 详见 [ADR-0010](./docs/adr/0010-codex-responses-response-shape-unverified.md)。

### 在线画图

- 生成、单图编辑、多图组图编辑
- 参考图上传、多图生成交互
- 本地保存会话历史（回看 / 删除 / 清空）
- 服务端图片 URL 缓存、进度追踪、超时后可继续等待
- 图片懒加载与滚动位置记忆

### 号池管理

- 自动刷新账号邮箱、类型、额度、恢复时间（异步进度追踪）
- 按槽位轮询账号执行生图 / 编辑（每账号并发可配，默认 3）
- Token 失效（401/403）自动剔除；429 限流不误删
- 定时检查限流账号并刷新
- **密码重新登录**（含 TOTP 两步验证）恢复异常账号
- 全局 HTTP / HTTPS / SOCKS5 / SOCKS5H 代理配置
- WARP / FlareSolverr 代理运行时
- 搜索、筛选、批量刷新、导出、手动编辑、清理
- 四种导入方式：本地 CPA JSON、远程 CPA 服务器、`sub2api` 服务器、`access_token`
- 图片存储三模式：`local` / `webdav` / `both`（远端失败降级为本地）

已知限制见 [ADR-0009](./docs/adr/0009-go-rewrite-scope-and-gaps.md) 的「遗留的已知限制」。
## API

所有 `/v1/*` 与 `/api/*` 接口都需要请求头：

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
| `model` | 以 `GET /v1/models` 返回为准，推荐 `gpt-image-2` |
| `prompt` | 提示词 |
| `n` | 生成数量 `1`–`4`。**越界返回 422，不静默夹取** |
| `response_format` | `b64_json`（默认）或 `url` |
| `stream` | `true` 时走 SSE；失败会在流内补一条 `error` 事件再 `[DONE]` |

### 图片编辑

支持 multipart（官方形态）与 JSON（data URL）：

```bash
curl http://localhost:3000/v1/images/edits \
  -H "Authorization: Bearer <auth-key>" \
  -F "model=gpt-image-2" \
  -F "prompt=把背景换成海边" \
  -F "image=@input.png"
```

`n` 越界返回 400（对齐 Python 的 `_parse_count`）。`http(s)://` 图片引用返回 400
—— 本项目不代拉远程图片。

### 异步任务（PPT / PSD / 大图）

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

产物落在 `data/files/ppt/<task_id>/`，通过 `/files/ppt/<task_id>/<文件名>` 访问。

> `client_task_id` 省略时服务端生成 UUID。它是**幂等键**，同一 owner 重复提交同一 id 会复用任务。

### 管理面接口

共 77 条路由（GET 30 / POST 42 / DELETE 5）。完整清单以 `internal/api/*_routes.go` 为准（[路由注册表](https://github.com/basketikun/chatgpt2api/tree/main/internal/api)）。
常用几组：

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

需要 **Go 1.27+** 与 **Node 22+**。

```bash
git clone https://github.com/basketikun/chatgpt2api.git
cd chatgpt2api

# web_dist 目录里有 .gitkeep 占位，所以不构建前端也能编译。
go build -o chatgpt2api ./cmd/server
CHATGPT2API_AUTH_KEY=dev-secret ./chatgpt2api --addr :8000
```

> 源码运行请用 **`:8000`**：前端开发服务硬编码指向 `http://127.0.0.1:8000`
> （见 `web/src/constants/common-env.ts`）。Docker 部署是另一套端口
> —— 容器内 8080、对外 3000，由 compose 映射，互不影响。

前端开发（热重载，含对后端 `:8000` 的代理）：

```bash
cd web && npm install && npm run dev
```

让后端直接使用未 embed 的前端产物（不跑 dev server）：

```bash
cd web && npm run build                    # 产物在 web/out
CHATGPT2API_WEB_DIST=./web/out CHATGPT2API_AUTH_KEY=dev ./chatgpt2api --addr :8000
```

### 测试

```bash
go test ./...                                 # 561 个：单元 + golden + 路由对齐
go test -race ./...                           # 竞态检测
go test -tags live ./internal/upstream/ -v    # 真实上游握手（需外网）
```

`-tags live` 的用例只记录上游状态、不设失败断言 —— 上游变了应该被观察，而不是让 CI 红。

覆盖率（语句）：web 91.5% · health 88.5% · protocol 78.2% · config 64.9% ·
upstream 55.8% · service 53.0% · storage 44.1% · api 42.0%。

PostgreSQL 后端测试需设置 `TEST_POSTGRES_URL`，未设置时跳过。

### 发布

打 tag 触发两条独立流水线：

```bash
git tag v2.0.1 && git push origin v2.0.1
```

- [`.github/workflows/release.yml`](./.github/workflows/release.yml) — 跨平台编译二进制、生成 `SHA256SUMS.txt`、建 GitHub Release
- [`.github/workflows/docker-publish.yml`](./.github/workflows/docker-publish.yml) — 构建 amd64/arm64 镜像推到 GHCR

`release.yml` 也可以手动触发（`workflow_dispatch`）只跑构建、不建 Release。

---

## 文档索引

| 文档 | 内容 |
|:--|:--|
| [部署文档](./docs/deployment.md) | 部署形态、反向代理、存储后端、备份、升级与回滚 |
| [功能清单](./docs/feature-status.en.md) | 逐项功能状态 |
| [ADR](./docs/adr/) | 12 条架构决策记录（重构范围、错误信封、数据格式、并发模型…） |
| [上游 SSE 协议](./docs/upstream-sse-conversation.md) | 上游会话流式协议记录 |
| [术语表](./GLOSSARY.md) | 项目内术语 |
| [CHANGELOG](./CHANGELOG.md) | 版本变更 |

---

## 效果展示

<table width="100%">
  <tr>
    <td width="50%"><img src="https://i.ibb.co/Jj8nfwwP/image.png" alt="image" border="0"></td>
    <td width="50%"><img src="https://i.ibb.co/pqf235v/image-edit.png" alt="image edit" border="0"></td>
  </tr>
  <tr>
    <td width="50%"><img src="https://i.ibb.co/tPcqtVfd/chery-studio.png" alt="chery studio" border="0"></td>
    <td width="50%"><img src="https://i.ibb.co/PsT9YHBV/account-pool.png" alt="account pool" border="0"></td>
  </tr>
  <tr>
    <td width="50%"><img src="https://i.ibb.co/rRWLG08q/new-api.png" alt="new api" border="0"></td>
  </tr>
</table>


## 社区支持

学 AI , 上 L 站：[LinuxDO](https://linux.do)

## Contributors

感谢所有为本项目做出贡献的开发者：

<a href="https://github.com/basketikun/chatgpt2api/graphs/contributors">
  <img alt="Contributors" src="https://contrib.rocks/image?repo=basketikun/chatgpt2api" />
</a>

## Star History

[![Star History Chart](https://api.star-history.com/chart?repos=basketikun/chatgpt2api&type=date&legend=top-left)](https://www.star-history.com/?repos=basketikun%2Fchatgpt2api&type=date&legend=top-left)
