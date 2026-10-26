# 账号接口 —— 上传与代理配置

给注册机 / 自动化脚本用的接口说明。所有端点需要 **admin** 权限。

```bash
AUTH="Authorization: Bearer $CHATGPT2API_AUTH_KEY"
BASE="http://127.0.0.1:3000"
```

---

## 1. 上传账号

```
POST /api/accounts
```

### 请求体

两种形态可混用：

```json
{
  "tokens":   ["eyJhbGci...", "eyJhbGci..."],
  "accounts": [ { "access_token": "eyJhbGci...", "...": "..." } ]
}
```

`tokens` 是纯 access_token 数组，`accounts` 是完整账号对象数组。两者可以同时出现，
重复的 access_token 会自动去重。

### 账号对象字段

| 字段 | 必填 | 说明 |
|:--|:--|:--|
| `access_token` | ✅ | 上游 JWT。导入后用于所有上游请求 |
| `account_id` | | **稳定标识**，不传会自动生成。外部系统用它做引用 |
| `refresh_token` | | 有它才能走 RT 续期（不用重新登录） |
| `id_token` | | 导出/迁移时带上更完整 |
| `email` | | 不填会在首次刷新时从上游补齐 |
| `password` | | 有它才能在失效后自动密码重登 |
| `totp_secret` | | 账号开了 TOTP 两步验证时**必须**提供，否则重登失败 |
| `proxy` | | 该账号的专属出口代理，见第 3 节 |

`password` + `totp_secret` 是「账号被救回来」的前提：RT 失效后唯一出路是重新登录。
只传 access_token 的账号一旦失效就只能人工处理。

### 2FA 要点

**只支持 TOTP（认证器 App）**，不支持短信/邮箱验证码/硬件密钥。

| 账号情况 | 要带什么 |
|:--|:--|
| 无 2FA | 只需 `password` |
| TOTP 2FA | `password` + `totp_secret`（Base32，6 位/30 秒） |
| 邮箱验证码 2FA | 无法自动重登，导入了也是一次性 |

`totp_secret` 的容错很宽：大小写、空格、连字符、`=` 填充都能接受。
`JBSW Y3DP EHPK 3PXP` 与 `jbswy3dpehpk3pxp` 等价。

漏了 `totp_secret` 不会导致导入失败，但**该账号 AT 失效后无法自动恢复** ——
停在「异常」等人工处理。可以用 `POST /api/accounts/update` 事后补。

细节见[格式参考的 2FA 章节](./account-api-reference.md#65-两步验证2fa--totp)。

### 返回

```json
{
  "added": 2,
  "skipped": 0,
  "refreshed": 0,
  "refresh_deferred": 2,
  "errors": [],
  "items": [ ... ]
}
```

| 字段 | 含义 |
|:--|:--|
| `added` | 新增数量 |
| `skipped` | 已存在而跳过的数量（**密码/代理等字段仍会更新**） |
| `refreshed` | 本次同步刷新的数量（默认 0） |
| `refresh_deferred` | 交给后台 watcher 刷新的数量 |
| `errors` | 刷新失败明细（仅 `?refresh=1` 时非空） |

### 刷新行为：默认不阻塞

上传**不会**同步刷新账号，立即返回。刷新由后台 watcher 完成（启动后先跑一轮，
之后按 `refresh_account_interval_minute` 周期跑）。

原因：同步刷新会给每个账号打三次上游请求（`/me` + `conversation/init` +
`accounts/check`）。500 个账号串行要几分钟，且上游会看到 500 个新会话。

如果确实要在返回前验证 token 有效性：

```
POST /api/accounts?refresh=1
```

`refresh=1` / `true` / `yes` 均可。此时 `refreshed` 是实际成功数，`errors` 列出失败明细。

### 上传后账号字段是空的？

正常。`email`、`quota`、`limits_progress` 要等后台刷新后才填上。
想立刻看到，用 `?refresh=1`，或手动 `POST /api/accounts/refresh`。

### 示例

```bash
curl -sS -X POST "$BASE/api/accounts" \
  -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{
    "accounts": [{
      "access_token":  "eyJhbGci...",
      "refresh_token": "rt.1.AACD...",
      "email":         "user@example.com",
      "password":      "P@ssw0rd",
      "totp_secret":   "JBSWY3DPEHPK3PXP",
      "proxy":         "socks5h://user:pass@10.0.0.1:1080"
    }]
  }'
```

### 批量上限

请求体上限 8 MiB。实测约 3.5 KB/账号，**可容纳约 2400 个账号**。
超过请分批（每批 1000 个留足余量）。

---

## 2. 编辑单个账号

```
POST /api/accounts/update
```

```json
{
  "access_token": "eyJhbGci...",
  "proxy": "socks5h://user:pass@10.0.0.2:1080"
}
```

可改字段：`type`、`status`、`quota`、`proxy`。其余字段会被忽略。

返回 `{"item": {...}, "items": [...]}`。

---

## 3. 批量设置代理

```
POST /api/accounts/proxy
```

### 形态 A —— 统一代理

```json
{
  "access_tokens": ["at-1", "at-2", "at-3"],
  "proxy": "socks5h://user:pass@10.0.0.1:1080"
}
```

### 形态 B —— 逐个映射

```json
{
  "proxies": {
    "at-1": "socks5h://user:pass@10.0.0.1:1080",
    "at-2": "http://10.0.0.2:8080",
    "at-3": ""
  }
}
```

映射里的空字符串表示**清空**该账号的代理，回退到全局代理。

### 返回

```json
{ "updated": 3, "missing": [], "items": [ ... ] }
```

`missing` 列出找不到的 access_token。

---

## 4. 代理如何生效

### 优先级

```
账号自己的 proxy  →  config.json 的全局 proxy  →  直连
```

账号没配（或配成空白字符）时自动回退全局。

### 覆盖范围

账号级代理对以下出站请求生效：

- 账号信息刷新（`/me`、`conversation/init`、`accounts/check`）
- 文本对话（`/v1/chat/completions`、`/v1/responses`、`/v1/messages`）
- 图片生成与编辑
- 搜索
- PPT / PSD 生成
- 模型目录拉取（`/v1/models`）

**例外**：密码重登走全局代理。原因是登录时手上只有邮箱和密码，
要等登录成功拿到 token 才知道是哪个账号。

### 支持的代理格式

| 格式 | 示例 |
|:--|:--|
| SOCKS5（远端解析 DNS） | `socks5h://user:pass@host:1080` |
| SOCKS5 | `socks5://user:pass@host:1080` |
| HTTP | `http://user:pass@host:8080` |
| HTTPS | `https://user:pass@host:8443` |

`socks5h://` 的 `h` 表示 **DNS 也在代理侧解析** —— 对需要隐藏解析行为的场景用这个。

### 全局代理怎么设

`config.json`：

```json
{ "proxy": "socks5h://user:pass@10.0.0.1:1080" }
```

或环境变量 `CHATGPT2API_PROXY`。改完需要重启。

> **注意**：设置页里的 `proxy_runtime`（FlareSolverr / WARP 那一组）**不参与真实出站**，
> 它只影响 `/health` 与设置页的状态显示。真正决定出站的是账号 `proxy` 与全局 `proxy`。

---

## 5. 注册机典型流程

```bash
# 1. 注册机产出一个账号后立刻上传，并分配当次使用的代理
curl -sS -X POST "$BASE/api/accounts" \
  -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{
    \"accounts\": [{
      \"access_token\":  \"$AT\",
      \"refresh_token\": \"$RT\",
      \"email\":         \"$EMAIL\",
      \"password\":      \"$PASSWORD\",
      \"totp_secret\":   \"$TOTP\",
      \"proxy\":         \"$PROXY\"
    }]
  }"

# 2. 注册机跑完一批后，统一确认哪些 token 有效（可选）
curl -sS -X POST "$BASE/api/accounts/refresh" -H "$AUTH"

# 3. 给某批账号换代理（例如原代理被封）
curl -sS -X POST "$BASE/api/accounts/proxy" \
  -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"proxies": {"at-1": "socks5h://new:pass@10.0.0.9:1080"}}'
```

### 建议

1. **务必带 `password` + `totp_secret`**。RT 会失效，没有密码就只能人工救。
2. **每个账号配独立代理**，降低上游关联风险。
3. **上传时不要 `?refresh=1`**，除非确实需要立即验证。让后台批量刷。
4. **把返回的 `account_id` 存进你自己的库**。它是稳定引用键 —— `access_token`
   每 3 天轮换一次，用 token 做外键的话轮换后就对不上了。
5. **每批 ≤1000 个账号**，避免撞 8 MiB 请求体上限。

---

## 6. 相关端点速查

| 端点 | 用途 |
|:--|:--|
| `POST /api/accounts` | 上传账号（`?refresh=1` 可选同步刷新） |
| `GET /api/accounts` | 列出账号 |
| `POST /api/accounts/update` | 改单账号（`type`/`status`/`quota`/`proxy`） |
| `POST /api/accounts/proxy` | 批量设置代理 |
| `GET /api/accounts/detail` | **查看单账号详情**（凭据原文、AT 有效期、代理来源、诊断） |
| `POST /api/accounts/refresh` | 刷新账号信息（后台跑，返回 `progress_id`） |
| `GET /api/accounts/refresh/progress/:progress_id` | 查刷新进度 |
| `POST /api/accounts/re-login` | 密码重登（救失效账号） |
| `GET /api/accounts/re-login/progress/:progress_id` | 查重登进度 |
| `DELETE /api/accounts` | 删除账号 |
| `POST /api/accounts/export` | 导出账号（**含凭据原文**，用于备份/迁移） |
| `POST /api/accounts/oauth/start` | 走浏览器 OAuth 登录拿 token，返回 `session_id` 与 `authorize_url` |
| `POST /api/accounts/oauth/finish` | 用 OAuth 回调结果落库，返回新增账号（已脱敏） |
| `GET /version` | 当前版本号（无需鉴权） |

### 6.1 OAuth 登录（替代手工抓 token）

不用自己去浏览器 DevTools 复制 token，让服务代跑一遍：

```bash
# 1) 起一个会话，拿到授权链接
curl -sS -X POST http://127.0.0.1:3000/api/accounts/oauth/start \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"email_hint":"you@example.com"}'
# → {"session_id":"...","authorize_url":"https://auth.openai.com/..."}

# 2) 浏览器打开 authorize_url 完成登录，复制回调 URL

# 3) 把回调交回服务
curl -sS -X POST http://127.0.0.1:3000/api/accounts/oauth/finish \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"session_id":"...","callback":"http://localhost:1455/auth/callback?code=..."}'
```

`finish` 的响应里 `items` 是**脱敏**的（与 `GET /api/accounts` 一致）。

---

## 7. 常见问题

**Q：上传成功但账号列表里 `email` 是空的？**
A：刷新还没跑。等 watcher 下一轮，或 `POST /api/accounts/refresh`。

**Q：账号状态变成「异常」了？**
A：`/backend-api/me` 返回 401，token 真的失效了。如果账号有 `password`，
`auto_relogin_after_refresh` 开着会自动重登救回；否则只能重新导入。

**Q：配了账号代理但看起来没生效？**
A：检查代理格式（需要 scheme，如 `socks5h://`）。代理本身不可用时，
该账号的所有上游请求都会失败，表现为「异常」或 502。

**Q：`skipped` 大于 0，是不是没导入？**
A：access_token 已存在，跳过了新增。但 `password`、`proxy` 等字段**仍会被更新** ——
可以借此给已有账号补密码或改代理。
