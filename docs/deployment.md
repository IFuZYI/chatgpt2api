# 部署与升级指南

本文覆盖 ChatGPT2API 的全部部署方式、持久化数据、反向代理、存储与图片后端、
备份、升级回滚，以及安全加固与排障。

**先读这三条**：

1. `config.json` **不在仓库里**（避免把 WebDAV 密码、R2 密钥、代理凭据提交上去）。
   Docker 部署时必须从 `config.example.json` 复制一份（compose 的挂载要求文件存在）；
   直接跑二进制时它是**可选**的 —— 只设 `CHATGPT2API_AUTH_KEY` 也能启动，
   见 [配置来源与优先级](#配置来源与优先级)。
2. `auth-key` **留空服务会拒绝启动**（fail-closed，见 `internal/config/config.go`）。
   这是刻意的：空密钥永不匹配任何请求，与其让管理面静默全开放，不如直接不给起。
3. 本项目是**单实例应用**：本地状态 + 进程内任务队列，
   **不支持多实例或多副本水平扩展**，见 [ADR-0004](./adr/0004-single-instance-local-state.md)。

---

## 目录

- [持久化数据](#持久化数据)
- [方式一：Docker Compose（推荐）](#方式一docker-compose推荐)
- [方式二：单二进制](#方式二单二进制)
- [方式三：WARP / FlareSolverr 代理](#方式三warp--flaresolverr-代理)
- [方式四：源码运行](#方式四源码运行)
- [反向代理](#反向代理)
- [配置来源与优先级](#配置来源与优先级)
  （详细版见 [配置说明](./configuration.md)）
- [存储后端](#存储后端)
- [图片存储](#图片存储)
- [备份](#备份)
- [升级与回滚](#升级与回滚)
- [安全加固](#安全加固)
- [排障](#排障)
- [常用维护命令](#常用维护命令)

---

## 持久化数据

| 路径 | 作用 | 丢了会怎样 |
|:--|:--|:--|
| `config.json` | 主配置：认证密钥、代理、图片存储、备份、审核 | 服务拒绝启动（缺 auth-key） |
| `data/accounts.json` | 号池（含 token，`sqlite`/`postgres` 后端则为数据库） | 号池丢失，需重新导入 |
| `data/logs.jsonl` | 调用日志 | 历史记录丢失 |
| `data/images/` 与 `data/image_index.json` | 本地图片与索引 | 图片不可访问 |
| `data/files/` | PPT / PSD 产物 | 产物下载 404 |
| `data/image_tasks.json` | 图片任务记录 | 在途任务丢失 |
| `.env` | compose 环境变量（WARP 方式需要） | 端口/代理配置回默认 |

**升级和迁移时只需要保留以上内容**；二进制/镜像本身可以随时替换。

> `data/` 目录必须对容器内的 **uid 65532**（`nonroot`）可写。
> 用 bind mount 时若宿主机目录属主是 root 且权限过紧，
> 服务会在写账号文件时报 `permission denied`。

---

## 方式一：Docker Compose（推荐）

适合生产自托管。不需要 Cloudflare 清障时用这个。

### 准备

```bash
docker version            # 需要 Docker
docker compose version    # 需要 Compose v2

git clone https://github.com/IFuZYI/chatgpt2api.git
cd chatgpt2api
cp config.example.json config.json
```

编辑 `config.json`，至少设置：

```json
{
  "auth-key": "换成你自己的长随机串"
}
```

或用环境变量覆盖（优先级更高）：

```yaml
# docker-compose.yml
environment:
  - CHATGPT2API_AUTH_KEY=your_secret_key
```

> **不要照抄 `config.example.json` 的 `auth-key` 值。** 模板里是空串；
> 而如果你用了像 `chatgpt2api` 这样等于项目名的值，它会被任何读到仓库的人直接猜中。
> 用 `openssl rand -hex 32` 生成。

### 启动

```bash
docker compose up -d
docker compose logs -f
```

访问：

```text
Web 面板： http://localhost:3000
API 地址： http://localhost:3000/v1
```

> 容器内固定监听 **8080**：distroless 的 `nonroot`（uid 65532）没有
> `CAP_NET_BIND_SERVICE`，绑不上 <1024 的端口（`:80` 会直接
> `bind: permission denied` 起不来）。
> 对外端口由 `CHATGPT2API_PORT` 决定（默认 **3000**）—— 改它即可，不要改 Dockerfile。

### 端口与卷

```yaml
ports:
  - "${CHATGPT2API_PORT:-3000}:8080"
volumes:
  - ./data:/app/data
  - type: bind
    source: ./config.json
    target: /app/config.json
    bind:
      create_host_path: false   # 文件不存在直接报错，而不是建一个同名目录
```

`create_host_path: false` 是刻意的。短语法在源文件不存在时会**创建一个同名目录**，
服务于是读不到配置，而报出的错误是「config 是目录」—— 很难排查。

### 停机行为

```yaml
stop_grace_period: 45s
```

关闭流程大约需要 40s（`server.Shutdown` 10s + watcher/cleanup 停止 +
图片任务 `Close` 最多 30s）。Docker 默认只给 10s 就 `SIGKILL`，
会截断「在途任务写 `image_tasks.json`」并破坏 JSON 后端的原子写 —— 那是真的丢数据。
**不要把这个值调到低于 45s。**

### 健康检查

```yaml
healthcheck:
  test: ["CMD", "/app/chatgpt2api", "--healthcheck"]
  interval: 30s
  timeout: 5s
  retries: 3
  start_period: 20s
```

distroless 镜像里**没有 curl / wget**，所以用二进制自带的 `--healthcheck` 子命令。

---

## 方式二：单二进制

适合没有 Docker 的环境（边缘设备、NAS、已有 systemd 的服务器）。

从 [Releases](https://github.com/IFuZYI/chatgpt2api/releases) 下载对应平台的文件：

| 平台 | 文件后缀 |
|:--|:--|
| Linux x86_64 | `..._linux_amd64` |
| Linux ARM64 | `..._linux_arm64` |
| Linux ARMv7 | `..._linux_armv7` |
| macOS Intel / Apple Silicon | `..._darwin_amd64` / `..._darwin_arm64` |
| Windows x64 | `..._windows_amd64.exe` |

校验后运行：

```bash
sha256sum -c SHA256SUMS.txt
chmod +x chatgpt2api_*_linux_amd64

mkdir -p /opt/chatgpt2api && cd /opt/chatgpt2api
cp /path/to/config.example.json config.json
$EDITOR config.json            # 设置 auth-key
CHATGPT2API_AUTH_KEY=$(openssl rand -hex 32) \
  ./chatgpt2api_*_linux_amd64 --addr :8080
```

首次运行会在当前目录创建 `data/`。`config.json` **不会**被自动创建 —— 缺少它时
服务用空配置 + 环境变量启动（只要 `auth-key` 有值）。

想改配置项（图片保留天数、代理、备份等，这些**不支持**环境变量）就从模板复制一份：

```bash
curl -fsSLO https://raw.githubusercontent.com/IFuZYI/chatgpt2api/main/config.example.json
mv config.example.json config.json && $EDITOR config.json
```

用 `--base-dir` 或 `CHATGPT2API_BASE_DIR` 换数据目录。

### systemd 单元示例

```ini
# /etc/systemd/system/chatgpt2api.service
[Unit]
Description=ChatGPT2API
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=chatgpt2api
WorkingDirectory=/opt/chatgpt2api
ExecStart=/opt/chatgpt2api/chatgpt2api --addr :8080
Restart=always
RestartSec=5
# 给关闭流程留够时间（与 compose 的 stop_grace_period 同理）
TimeoutStopSec=60

# 凭据走环境变量，避免写进 unit 文件被其他用户读到
EnvironmentFile=/opt/chatgpt2api/.env
# 文件与目录权限收紧
UMask=0077

[Install]
WantedBy=multi-user.target
```

`/opt/chatgpt2api/.env`：

```bash
CHATGPT2API_AUTH_KEY=用 openssl rand -hex 32 生成
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now chatgpt2api
```

> 二进制**内置前端**（编译期 `go:embed`），不需要额外部署静态文件。
> 启动日志里的版本号来自 `-ldflags` 注入的 `config.BuildVersion`；
> 若旁边有 `VERSION` 文件则优先读它。

---

## 方式三：WARP / FlareSolverr 代理

上游请求经常遇到 Cloudflare 拦截时启用。这套组合会额外启动：

| 容器 | 作用 | 监听 |
|:--|:--|:--|
| `warp` | Cloudflare WARP 出口 | 内部 |
| `privoxy` | 把 WARP 的 SOCKS5 转成 HTTP 代理 | `127.0.0.1` |
| `flaresolverr` | 自动处理 Cloudflare 挑战 | `127.0.0.1` |
| `init-data` | 一次性容器，把 `./data` 属主改成 uid 65532 | — |
| `init-config` | 一次性容器，跑 `--init-proxy-config` 把 proxy_runtime 默认值写进 `config.json` | — |

```bash
cp .env.example .env
$EDITOR .env                   # 按需改端口与 WARP_LICENSE_KEY
docker compose -f docker-compose.warp.yml up -d --build
```

### init-config 做什么

它跑的是**同一个二进制**的子命令，不是外部脚本：

```bash
/app/chatgpt2api --init-proxy-config
```

行为：

1. 读 `CHATGPT2API_CONFIG_FILE`（compose 已设为 `/app/host/config.json`）。
   文件不存在就创建，**不要求先有 `auth-key`** —— 它只写配置、不启动服务。
2. 从一组 `CHATGPT2API_PROXY_RUNTIME_*` 环境变量推导出 `proxy_runtime` 段
   （`.env` 里能改，完整清单见 [`.env.example`](../.env.example)）。
3. 幂等合并：若现有 `proxy_runtime` 还是仓库默认值就整体替换，否则**只补齐
   缺失的键**，不动你手改过的值。想无条件覆盖设
   `CHATGPT2API_PROXY_RUNTIME_FORCE=true`。
4. 打印一行脱敏摘要后退出（URL 里的密码会变成 `[REDACTED]`）。

手动跑（不经过 compose）：

```bash
CHATGPT2API_CONFIG_FILE=./config.json \
CHATGPT2API_PROXY_RUNTIME_PROXY_URL=http://127.0.0.1:8118 \
  ./chatgpt2api --init-proxy-config
```

> ⚠️ **安全提示**：`warp` 容器需要 `NET_ADMIN` 与 `SYS_MODULE` 能力；
> `init-config` 与 `init-data` 都以 `user: "0:0"` 运行（前者要写宿主机上的
> `config.json`，后者要 chown `./data`）。这两个容器跑完即退出、不监听端口。
> 请确认宿主机目录权限，并注意 `.env` 里的凭据对其他容器可见
> （环境变量能通过 `docker inspect` 读到）。

> **未在本仓库验证**：完整的 WARP 链路需要真实 WARP 容器与 `NET_ADMIN`，
> 当前环境无法端到端跑通。已验证的是 `init-data` 与 `init-config` 两个
> 一次性容器（各自的日志与产物都核对过）。

用 `docker compose -f docker-compose.warp.yml logs -f` 看日志。

---

## 方式四：源码运行

适合本地开发。需要 **Go 1.27+** 与 **Node 22+**。

```bash
git clone https://github.com/IFuZYI/chatgpt2api.git
cd chatgpt2api

go build -o chatgpt2api ./cmd/server
CHATGPT2API_AUTH_KEY=dev-secret ./chatgpt2api --addr :8000
```

> 源码运行请用 **`:8000`**：前端 dev server 硬编码指向 `http://127.0.0.1:8000`
> （`web/src/constants/common-env.ts`）。Docker 那套（容器 8080 / 对外 3000）
> 是另一套端口，两者互不影响。

前端热重载：

```bash
cd web && npm install && npm run dev
```

让后端直接用未 embed 的前端产物：

```bash
cd web && npm run build                      # 产物在 web/out
CHATGPT2API_WEB_DIST=$PWD/web/out CHATGPT2API_AUTH_KEY=dev ./chatgpt2api --addr :8000
```

源码运行默认读取项目根目录的 `config.json`、`data/`、`VERSION`。

### 跑测试

```bash
go test ./...                                 # 561 个：单元 + golden + 路由对齐
go test -race ./...                           # 竞态检测
go test -tags live ./internal/upstream/ -v    # 真实上游握手（需外网）
```

PostgreSQL 后端测试需要 `TEST_POSTGRES_URL`，未设置时跳过。

---

## 反向代理

放在 nginx / Caddy / Traefik 后面时，注意三件事：

1. **SSE 不能被缓冲。** 生图与文本流式走 `text/event-stream`，
   nginx 默认会缓冲，导致客户端几十秒收不到任何字节、被自己的超时切断。
2. **流式请求需要更长的读超时。** 生图一次可达几十秒。
3. **转发 `Host` 与原协议头。** 生成的图片 URL 默认按请求的
   `Host` / `X-Forwarded-Proto` 拼；若代理没转发，链接会指向内部地址。
   更稳妥的做法是直接配 `base_url`（`config.json` 里的 `base_url`
   或环境变量 `CHATGPT2API_BASE_URL`），不要依赖请求头推导。

### nginx

```nginx
location / {
    proxy_pass http://127.0.0.1:3000;
    proxy_http_version 1.1;

    proxy_set_header Host              $host;
    proxy_set_header X-Real-IP         $remote_addr;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    # SSE：关缓冲、关缓存、拉长超时
    proxy_buffering off;
    proxy_cache off;
    proxy_read_timeout 300s;
    proxy_send_timeout 300s;
    chunked_transfer_encoding on;
}
```

### Caddy

```text
chatgpt2api.example.com {
    reverse_proxy 127.0.0.1:3000 {
        flush_interval -1        # SSE 需要：不缓冲
    }
}
```

Caddy 默认就会转发 `Host` 与 `X-Forwarded-Proto`。

> **建议**：反代场景下在 `config.json` 里显式设置 `base_url`
> （如 `https://chatgpt2api.example.com`）。否则生成的图片 URL 由请求头推导，
> 任何能发请求的人都能影响它写进日志管理台的那个地址。

---

## 配置来源与优先级

优先级：**环境变量 > `config.json` > 代码默认值**。

但**范围**要分清 —— 只有下列项支持环境变量：

| 项 | 环境变量 |
|:--|:--|
| 认证密钥 | `CHATGPT2API_AUTH_KEY` |
| 对外基础 URL | `CHATGPT2API_BASE_URL` |
| 数据根目录 | `CHATGPT2API_BASE_DIR` |
| 监听地址 | `CHATGPT2API_ADDR` |
| 诊断日志 | `CHATGPT2API_DEBUG` |
| 前端静态目录 | `CHATGPT2API_WEB_DIST` |
| 存储后端 | `STORAGE_BACKEND`、`DATABASE_URL` |
| git 后端 | `GIT_REPO_URL`、`GIT_TOKEN`、`GIT_BRANCH`、`GIT_FILE_PATH`、`GIT_AUTH_KEYS_FILE_PATH` |

**其余配置项（图片保留天数、代理运行时、备份、内容审核等）没有环境变量**，
只能写 `config.json`，或在 Web 面板的设置页里改（会写回 `config.json`）。
误以为「都能用环境变量设」是常见跳坑点。

### config.json 是可选文件

`config.json` 不存在时服务**不会报错**：`readJSONObject` 返回空 map，
配置全部走代码默认值 + 上述环境变量。所以最小部署可以不用这个文件：

```bash
CHATGPT2API_AUTH_KEY=$(openssl rand -hex 32) ./chatgpt2api --addr :8080
```

唯一硬性要求是 `auth-key` 必须有值（环境变量或文件二选一），
否则服务拒绝启动并给出明确提示。

> **注意**：服务**不会**自动创建 `config.json` —— 首次运行只创建 `data/`。
> 需要改配置项时从 `config.example.json` 复制一份。

### 用 Web 面板改配置

服务跑起来后，绝大多数配置项都能在设置页里改，保存后写回 `config.json`。
这也意味着**容器里必须让 `config.json` 可写**（compose 示例是 bind mount，
注意宿主机属主与权限，见 [安全加固](#安全加固)）。

## 存储后端

用 `STORAGE_BACKEND` 切换：

| 后端 | 说明 | 何时选它 |
|:--|:--|:--|
| `json` | 本地 JSON 文件（默认） | 账号少、单实例 |
| `sqlite` | 本地 SQLite（无 CGO 依赖） | 账号较多；比 JSON 省内存 |
| `postgres` | 外部 PostgreSQL | 需要外部备份或独立数据库 |
| `git` | Git 私有仓库 | 想用 git 做版本化备份 |

### SQLite

```yaml
environment:
  STORAGE_BACKEND: sqlite
  # 注意是 4 个斜杠：sqlite:/// 会当相对路径，服务随后报
  # "unable to open database file"，很难联想到是斜杠数量问题。
  DATABASE_URL: sqlite:////app/data/accounts.db
```

### PostgreSQL

```yaml
environment:
  STORAGE_BACKEND: postgres
  DATABASE_URL: postgresql://user:password@host:5432/dbname
```

### Git

```yaml
environment:
  STORAGE_BACKEND: git
  GIT_REPO_URL: https://github.com/your-name/your-private-repo.git
  GIT_TOKEN: ghp_xxxxxxxxxxxx
  GIT_BRANCH: main
  GIT_FILE_PATH: accounts.json
  GIT_AUTH_KEYS_FILE_PATH: auth_keys.json
```

> 只支持 `http`/`https`/`ssh`/`git`/`file` 传输。`ext::` 之类的
> 会被拒绝为 unsupported scheme。

---

## 图片存储

由 `image_storage.mode` 决定图片落在哪：

| 模式 | 行为 | 适用 |
|:--|:--|:--|
| `local`（默认） | 只写本地磁盘 | 单机、图不多 |
| `webdav` | 只写 WebDAV；失败即整次失败 | 想彻底不占本地盘 |
| `both` | 本地 + WebDAV | 兼顾可靠性（推荐） |

### `webdav` 与 `both` 的关键差别

- **`webdav`**：WebDAV 写失败 → 整次保存失败。静默退回本地会让用户以为图已在远端，
  而本地磁盘照样涨满，重启换机就丢图。
- **`both`**：WebDAV 写失败 → **降级为本地并返回成功**，索引如实标记
  `storage=local` / `webdav=false`。远端上传是**异步**的，不阻塞生图响应。
  见 [ADR-0011](./adr/0011-both-mode-degrades-on-webdav-failure.md) 与
  [ADR-0012](./adr/0012-both-mode-async-upload.md)。

### 配置

```json
{
  "image_storage": {
    "enabled": true,
    "mode": "both",
    "webdav_url": "https://webdav.example.com/dav",
    "webdav_username": "your-user",
    "webdav_password": "your-password",
    "webdav_root_path": "chatgpt2api/images",
    "public_base_url": ""
  }
}
```

> `mode` 设为 `webdav`/`both` 时**必须填 `webdav_password`**，
> 否则保存设置会返回 400。这是有意的前置校验，不是 bug。

用 Web 面板的「图片存储」页可以测试连通性、手动同步、按目标清理磁盘。

### 图片保留

`image_retention_days`（代码默认 **30** 天；但 `config.example.json` 里写的是 **15**，
照模板部署会得到 15 天 —— 想用 30 就改这一项或删掉它）控制本地图片与任务记录的清理，
由内置清理调度器执行。

### 图片也能存到 Cloudflare R2 吗

R2 是**备份**用的（见下节），不是图片热存储。图片热存储只有 local / WebDAV 两条路。

---

## 备份

内置备份会把配置与数据打包上传到 Cloudflare R2。

```json
{
  "backup": {
    "enabled": true,
    "provider": "cloudflare_r2",
    "account_id": "your-account-id",
    "bucket": "your-bucket",
    "access_key_id": "your-key-id",
    "secret_access_key": "your-secret",
    "prefix": "backups",
    "interval_minutes": 360,
    "rotation_keep": 10,
    "encrypt": true,
    "passphrase": "设置一个强口令"
  }
}
```

### ⚠️ 归档默认不加密，且内含全部凭据

归档里包含：

- `config.json` —— **admin auth-key、WebDAV 密码、R2 密钥本身**
- `snapshots/accounts.json` —— **号池全部 access_token**
- `snapshots/auth_keys.json` —— 用户密钥哈希
- `data/logs.jsonl`、`data/image_tasks.json`

**`encrypt` 默认是 `false`，`passphrase` 默认是空的。** 也就是说默认产物是一个
明文 tar.gz，落在第三方对象存储上。这份归档等价于「凭据恢复套件」。

部署前请务必：

1. 设 `"encrypt": true` 并填一个强 `passphrase`；
2. 确认 bucket **不是公开可读**，并单独用一个只对该 bucket 有权限的凭据；
3. 定期验证归档能下载、能解开（`tar -tzf` 应能看到上述文件）。

> `rotation_keep` 控制保留几份；`interval_minutes` 代码默认 **360**（6 小时）。
> 也可以只开手动：关掉 `enabled`，在面板里点「立即备份」。

---

## 升级与回滚

### 升级前一定先备份

```bash
cd /path/to/chatgpt2api
tar czf "backup-$(date +%Y%m%d-%H%M%S).tar.gz" config.json data/ .env
```

### Docker 方式

```bash
docker compose pull
docker compose up -d
docker compose logs -f --tail=50
```

### 单二进制方式

```bash
systemctl stop chatgpt2api
cp chatgpt2api chatgpt2api.bak          # 留一份旧二进制
curl -L -o chatgpt2api.new <新版本链接>
chmod +x chatgpt2api.new && mv chatgpt2api.new chatgpt2api
systemctl start chatgpt2api
```

### 回滚

```bash
docker compose down
docker compose up -d --force-recreate <指定旧镜像 tag>
```

或源码方式回到旧 tag 重新编译：

```bash
git fetch --tags && git checkout v2.0.0 && go build -o chatgpt2api ./cmd/server
```

数据回滚用升级前那个 tar 包。**注意**：数据格式向上兼容，但降级时若新版本
引入过新字段，旧版本会忽略它们（不会崩），反之未必。

---

## 安全加固

这一节来自一轮完整的安全审计，都是实测结论。

### 必做

1. **用强 `auth-key`。** 它就是超级管理员密钥。
   不要用项目名、不要用 `admin`。`openssl rand -hex 32`。
   历史上有部署用 `chatgpt2api` 作为密钥，而这个值出现在仓库的示例文件里 —— 等于公开。
2. **`config.json` 与 `data/` 不要暴露给其他本地用户。**

   ```bash
   chmod 700 /path/to/chatgpt2api
   chmod 600 /path/to/chatgpt2api/config.json
   ```

   > 服务在保存设置时会以 `0644` 重写 `config.json`，把操作者设的更严权限覆盖掉。
   > 所以更可靠的做法是收紧**父目录**权限（上面的 `chmod 700`）。

3. **管理面不要直接暴露公网。** 至少加一层反代 + IP 白名单 / Basic Auth，
   或只在内网监听。
4. **备份开加密**（见上节）。
5. **`proxy_runtime.skip_ssl_verify` 保持 `false`。** 设为 `true` 会关闭
   上游 TLS 校验。

### 建议

- 定期轮换 `auth-key` 与 WebDAV / R2 凭据。
- 若号池账号带 `password` / `totp_secret`（密码重登功能用），
  这些字段等同于账号全权凭据 —— 迁移时按凭据对待。
  `/api/accounts` 列表已对它们脱敏（只返回 `password_set` 布尔值），
  但**导出与备份里是明文的**，这是有意为之（否则导出无法恢复号池）。
- 不要用你的主力账号 / 高价值账号做测试。

### 已知的边界

- 上游错误文本未脱敏，会跨到普通用户（`/v1/*` 的 502 消息）并写入日志。
- `/health?format=json` 未鉴权，会返回号池规模与绝对文件路径。
- 后台任务不可取消：`Close` 只能等满超时。

完整清单见 [ADR-0009](./adr/0009-go-rewrite-scope-and-gaps.md) 的「遗留的已知限制」。

---

## 排障

### 服务起不来，日志说 `auth-key 未设置`

```text
config: ❌ auth-key 未设置！
```

设 `config.json` 的 `auth-key`，或给环境变量 `CHATGPT2API_AUTH_KEY`。

### `docker compose up` 报 config.json 相关的错

```
failed to mount ... no such file or directory
```

`config.json` 不存在。`create_host_path: false` 会直接报错而不建目录：

```bash
cp config.example.json config.json && $EDITOR config.json
```

### 容器起来了但写数据报 `permission denied`

**三个 compose 都已内置 `init-data` 一次性容器自动处理这件事**，正常流程不需要
手动介入。它的作用是把 `./data` 的属主改成容器内运行用户：

```yaml
init-data:
  image: alpine:3.20
  user: "0:0"
  command: ["sh", "-c", "mkdir -p /data && chown -R 65532:65532 /data"]
  volumes:
    - ./data:/data
```

> **为什么必须这么做**：镜像以 distroless `nonroot`（uid 65532）运行，而
> `./data` 由 Docker 首次 `up` 时创建为 `root:root 755`。此时服务能启动、
> `/health` 返回 200、读接口也正常 —— 但**一写数据就失败**：
>
> ```
> [account] save accounts failed: open /app/data/accounts.json.tmp: permission denied
> ```
>
> 「看起来健康、实际存不了东西」是最难排查的一类故障，所以做成默认行为。

如果你用 `docker run` 而不是 compose，或者想手动处理：

```bash
sudo chown -R 65532:65532 data/
# 或放宽到该用户可写
sudo chmod -R 775 data/
```

验证属主是否正确：

```bash
ls -ld data     # 期望 65532:65532
```

### 面板能开但接口 401

`auth-key` 不对，或 `Authorization` 头格式不对。格式必须是：

```http
Authorization: Bearer <auth-key>
```

### 生图几十秒后断开

多半是反代缓冲了 SSE。见 [反向代理](#反向代理) 一节的 `proxy_buffering off`。

### 图片点开 404

`webdav` 或 `both` 模式下，索引若与远端不一致会导致回源失败。
在面板「图片存储」页跑一次「测试」和「同步」。

### 图片 URL 指向奇怪的主机名

`base_url` 没配，服务回退到按请求的 `Host` 头拼接。
显式设置 `base_url` 或 `CHATGPT2API_BASE_URL`。

### 关闭时日志出现 `close timed out`

在途图片任务没能在 30s 内结束。确认 `stop_grace_period` / `TimeoutStopSec`
不低于 45s / 60s。这是已知限制（后台任务不可取消），见 ADR-0009。

### 想看更详细的生图日志

```bash
CHATGPT2API_DEBUG=1 docker compose up -d
```

会在 stderr 打 `[image]` 诊断日志。

---

## 常用维护命令

```bash
# 日志
docker compose logs -f --tail=100
docker logs -f chatgpt2api

# 重启
docker compose restart

# 停 / 删（保留数据）
docker compose stop
docker compose down

# 看资源占用
docker stats chatgpt2api

# 进容器（distroless 没有 shell，只能用自带子命令）
docker exec chatgpt2api /app/chatgpt2api --healthcheck

# 数据体积
du -sh data/ data/images data/files

# 备份
tar czf "backup-$(date +%Y%m%d-%H%M%S).tar.gz" config.json data/ .env
```

---

## 相关文档

- [README](../README.md) —— 快速开始与 API 概览
- [ADR 目录](./adr/) —— 架构决策记录
- [功能清单](./feature-status.en.md) —— 逐项功能状态
- [术语表](../GLOSSARY.md)