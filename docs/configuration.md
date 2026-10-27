# 配置文件、环境变量与 Docker Compose 的关系

**一句话**：部署时**只改 `config.json`**；环境变量只用于少数几个覆盖项，
`.env` 只是给 compose 做变量替换用的，不是应用配置。

---

## 三个文件各是什么

| 文件 | 角色 | 应用会读它吗 |
|:--|:--|:--|
| `config.example.json` | **模板**。复制成 `config.json` 后填值 | ❌ 完全不读 |
| `.env.example` | **模板**。复制成 `.env` 给 compose 用 | ❌ 完全不读 |
| `docker-compose.yml` | 部署编排（端口、卷、依赖） | ❌ 应用不读，Docker 读 |

真正的运行时配置只有 **`config.json`** 一个文件。
另外还有一层：**环境变量**，但它只覆盖一小部分配置项（见下）。

---

## 优先级（实测确认）

```
环境变量  >  config.json  >  代码默认值
```

但这句概括容易误解。准确的说法是：**只有少数几个键支持环境变量**，
其余键环境变量完全无效。

### 支持环境变量的键

| 配置项 | 环境变量 | 说明 |
|:--|:--|:--|
| 认证密钥 | `CHATGPT2API_AUTH_KEY` | 实测：env 有值时文件值失效 |
| 对外基础 URL | `CHATGPT2API_BASE_URL` | 实测：env 为空时用文件值 |
| 数据根目录 | `CHATGPT2API_BASE_DIR` | |
| 监听地址 | `CHATGPT2API_ADDR` | |
| 诊断日志 | `CHATGPT2API_DEBUG` | |
| 前端静态目录 | `CHATGPT2API_WEB_DIST` | |
| 存储后端 | `STORAGE_BACKEND` | |
| 数据库连接 | `DATABASE_URL` | |
| git 后端 | `GIT_REPO_URL`、`GIT_TOKEN`、`GIT_BRANCH`、`GIT_FILE_PATH`、`GIT_AUTH_KEYS_FILE_PATH` | |
| 配置文件路径 | `CHATGPT2API_CONFIG_FILE` | 默认 `<base-dir>/config.json` |
| 图片像素上限 | `CHATGPT2API_MAX_IMAGE_PIXELS` | 默认 2000 万；**仅环境变量可设** |
| 同时合成数 | `CHATGPT2API_MAX_CONCURRENT_COMPOSITES` | 默认 1，与内存预算联动；**仅环境变量可设** |

### 不支持环境变量的键

其余**全部**配置项只能写在 `config.json` 里，或在 Web 面板的设置页改
（面板保存会写回 `config.json`）。例如：

- `image_retention_minutes`、`image_account_concurrency`、`image_poll_timeout_secs`
- `proxy`（全局出口代理）
- `proxy_runtime.*`（WARP / FlareSolverr 代理运行时，**注意见下方例外**）
- `backup.*`（备份与加密）
- `sensitive_words`、`ai_review.*`（内容审核）
- `chat_completion_cache.*`、`log_levels`、`default_upstream_model_name`

> **实测反证**：设 `CHATGPT2API_IMAGE_RETENTION_DAYS=99` 启动后，
> `/api/settings` 里仍是代码默认值 30 —— 环境变量对这个键无效。

---

## `.env` 到底控制什么

`.env` **不是应用配置**，它只被 Docker Compose 用于**变量替换**。

三套 compose 都用 `environment:` 白名单把变量传进容器，**没有用 `env_file`**。
所以 `.env` 里写的东西，只有被 compose 的 `environment:` 段引用到的才会进容器。

实测：在 `.env` 里写 `IMAGE_RETENTION_DAYS=99` 和
`CHATGPT2API_PROXY_RUNTIME_ENABLED=true`，`docker compose config` 显示
它们**没有出现在容器的 environment 里** —— 只有白名单里的 9 个变量进去了。

### `.env` 能控制什么

| 变量 | 作用 |
|:--|:--|
| `CHATGPT2API_PORT` | 宿主机端口（默认 3000；local compose 是 8000） |
| `CHATGPT2API_AUTH_KEY` | 认证密钥（覆盖 config.json） |
| `CHATGPT2API_BASE_URL` | 对外基础 URL |
| `STORAGE_BACKEND` / `DATABASE_URL` | 存储后端 |
| `GIT_*` | git 后端 |
| `WARP_SOCKS_PORT` / `PRIVOXY_PORT` / `FLARESOLVERR_PORT` / `TZ` / `WARP_LICENSE_KEY` | 仅 warp compose 用 |

### 例外：`CHATGPT2API_PROXY_RUNTIME_*` 走的是子命令

`.env.example` 里那一大组 `CHATGPT2API_PROXY_RUNTIME_*` 由
`chatgpt2api --init-proxy-config` 消费（warp compose 的 `init-config` 容器
会自动跑）。它们把值**写进 `config.json`**，之后应用读的是文件，不是变量。

也就是说这组变量的作用是「**帮你生成 config.json 的 proxy_runtime 段**」，
而不是「运行时覆盖」。想改这些值又不想重跑 init 容器，直接改 `config.json`
或在 Web 面板改。

---

## 部署时该改哪个

### Docker 部署（三种 compose 都一样）

```bash
# 1. 必须：生成 config.json 并填 auth-key
cp config.example.json config.json
$EDITOR config.json          # 至少填 "auth-key"

# 2. 可选：改端口
cp .env.example .env
$EDITOR .env                 # 改 CHATGPT2API_PORT

# 3. 其余配置项：改 config.json，或启动后在 Web 面板改
```

**只改 `config.json` 就够了**。`.env` 只在需要改端口或换存储后端时才用。

### 单二进制 / 源码运行

```bash
# 方式 A：config.json
cp config.example.json config.json && $EDITOR config.json
./chatgpt2api --addr :8080

# 方式 B：纯环境变量（不用 config.json）
CHATGPT2API_AUTH_KEY=$(openssl rand -hex 32) ./chatgpt2api --addr :8080
```

方式 B 只能设置上面表格里的那几项。想改图片保留天数之类的，必须用方式 A
或启动后在面板改。

---

## 常见误解

| 误解 | 实际 |
|:--|:--|
| 「`.env` 里写什么都能生效」 | 只有 compose `environment:` 白名单里的才会进容器 |
| 「环境变量能覆盖所有配置项」 | 只有 9 类键支持，其余无效 |
| 「`config.example.json` 是给程序读的」 | 它只是模板，程序只读 `config.json` |
| 「改了 `.env` 就不用改 config.json」 | 只有端口、auth-key、存储后端等少数项能这么干 |
| 「`config.json` 必须存在」 | 不必须。缺失时用空配置 + 环境变量启动（但 `auth-key` 必须有值） |

---

## 验证方法

想确认某个配置项最终生效的值：

```bash
# 1. 看接口返回的解析后配置
curl -H "Authorization: Bearer <auth-key>" http://localhost:3000/api/settings

# 2. 看 compose 实际注入容器的环境变量
docker compose config | sed -n '/environment:/,/image:/p'

# 3. 看容器内实际的环境变量
docker exec chatgpt2api env    # 注意 distroless 没有 env 命令，改用：
docker inspect chatgpt2api --format '{{range .Config.Env}}{{println .}}{{end}}'
```
