# 账号接口 —— 格式参考

字段级参考。要「怎么做」看 [账号接口](./account-api.md)，这里只讲**格式**。

- 所有端点需要 **admin** 权限：`Authorization: Bearer <AUTH_KEY>`
- 请求体必须是 `application/json`
- 时间字段格式统一为 `YYYY-MM-DD HH:MM:SS`（**本地时区**，无时区后缀）

---

## 目录

1. [账号对象完整字段](#1-账号对象完整字段)
2. [上传：POST /api/accounts](#2-上传post-apiaccounts)
3. [编辑：POST /api/accounts/update](#3-编辑post-apiaccountsupdate)
4. [批量代理：POST /api/accounts/proxy](#4-批量代理post-apiaccountsproxy)
5. [代理字符串格式](#5-代理字符串格式)
6. [枚举值](#6-枚举值)
7. [错误响应](#7-错误响应)
8. [限制与上限](#8-限制与上限)

---

## 1. 账号对象完整字段

`GET /api/accounts` 返回的每个对象包含以下字段。上传时**只有 `access_token` 必填**，
其余都可以不传（服务端会给默认值或留空等待刷新补齐）。

### 上传时可写字段

| 字段 | 类型 | 默认 | 说明 |
|:--|:--|:--|:--|
| `access_token` | string | **必填** | 上游 JWT。空字符串会被整条丢弃 |
| `account_id` | string | 自动生成 | **稳定标识**，终身不变。见 [2.6](#26-account_id稳定标识)。不传则确定性派生 |
| `refresh_token` | string | `""` | 有它才能走 RT 续期，不必重新登录 |
| `id_token` | string | `""` | 导出/迁移时带上更完整 |
| `email` | string | `""` | 不填会在首次刷新时从上游补齐 |
| `password` | string | `""` | 有它才能在失效后自动密码重登 |
| `totp_secret` | string | `""` | Base32 TOTP 密钥。账号开了 2FA 时**必须**给 |
| `proxy` | string | `""` | 该账号专属出口代理，空 = 用全局 |
| `type` | string | `"free"` | 套餐类型，见[枚举](#6-枚举值) |
| `source_type` | string | `"web"` | 账号来源，见[枚举](#6-枚举值) |

### 只读字段（上传时传了会被忽略）

| 字段 | 类型 | 说明 |
|:--|:--|:--|
| `user_id` | string\|null | 上游用户 ID，刷新时补齐 |
| `quota` | int | 图片剩余额度，由刷新写入 |
| `limits_progress` | array | 各类额度明细，由刷新写入 |
| `restore_at` | string\|null | 额度恢复时间（RFC3339） |
| `default_model_slug` | string\|null | 上游默认模型 |
| `status` | string | 账号状态，见[枚举](#6-枚举值) |
| `created_at` | string | 创建时间（本地时区） |
| `success` / `fail` | int | 调用计数 |
| `invalid_count` | int | 连续失效次数（驱动异常账号退避） |
| `image_inflight` | int | 在途图片请求数 |
| `last_invalid_at` | string\|null | 最后一次失效时间 |
| `last_refresh_error` | string\|null | 最后一次刷新失败原因 |
| `last_refresh_error_at` | string\|null | 上述时间 |
| `last_token_refresh_at` | string\|null | 最后一次 RT 换 AT 成功时间（keepalive 锚点） |
| `last_token_refresh_error` | string\|null | 最后一次 RT 续期失败原因 |
| `last_token_refresh_error_at` | string\|null | 上述时间 |

### 完整样例（真实返回，凭据已脱敏）

```json
{
  "access_token": "eyJhbGciOiJSUzI1Ni...",
  "refresh_token": "rt.1.AACD_3vui...",
  "id_token": "eyJhbGciOiJSUzI1Ni...",
  "email": "user@example.com",
  "password": "P@ssw0rd",
  "totp_secret": "JBSWY3DPEHPK3PXP",
  "proxy": "socks5h://user:pass@10.0.0.1:1080",
  "type": "free",
  "source_type": "web",
  "user_id": "user-abc123",
  "quota": 23,
  "limits_progress": [
    {
      "feature_name": "image_gen",
      "remaining": 23,
      "reset_after": "2026-11-24T16:56:28.369324+00:00"
    }
  ],
  "restore_at": "2026-11-24T16:56:28.369324+00:00",
  "default_model_slug": "auto",
  "status": "正常",
  "created_at": "2026-10-26 05:22:23",
  "success": 0,
  "fail": 0,
  "invalid_count": 0,
  "image_inflight": 0,
  "last_invalid_at": null,
  "last_refresh_error": null,
  "last_refresh_error_at": null,
  "last_token_refresh_at": null,
  "last_token_refresh_error": null,
  "last_token_refresh_error_at": null
}
```

---

## 2. 上传：`POST /api/accounts`

### 请求体

三个键都可选，但至少要有一个能产出 access_token：

| 键 | 类型 | 说明 |
|:--|:--|:--|
| `tokens` | `string[]` | 纯 access_token 列表 |
| `accounts` | `object[]` | 完整账号对象列表（字段见上） |

```json
{
  "tokens": ["eyJhbGci...", "eyJhbGci..."],
  "accounts": [
    {
      "access_token": "eyJhbGci...",
      "refresh_token": "rt.1...",
      "email": "a@example.com",
      "password": "pw",
      "totp_secret": "JBSWY3DPEHPK3PXP",
      "proxy": "socks5h://u:p@10.0.0.1:1080"
    }
  ]
}
```

两者可同时出现，**同一 access_token 只计一次**（服务端去重）。

### 查询参数

| 参数 | 取值 | 默认 | 说明 |
|:--|:--|:--|:--|
| `refresh` | `1` / `true` / `yes` | 关 | 上传后同步刷新（阻塞，逐账号打上游） |

### 返回

```json
{
  "added": 2,
  "skipped": 1,
  "refreshed": 0,
  "refresh_deferred": 3,
  "errors": [],
  "items": [ /* 全部账号对象 */ ]
}
```

| 字段 | 类型 | 说明 |
|:--|:--|:--|
| `added` | int | 新增数量 |
| `skipped` | int | access_token 已存在而跳过新增（**其它字段仍会更新**） |
| `refreshed` | int | 本次同步刷新成功的数量（默认 0） |
| `refresh_deferred` | int | 交给后台 watcher 刷新的数量（去重后） |
| `errors` | array | 刷新失败明细；默认 `[]` |
| `items` | array | 全部账号对象 |

### `errors` 元素格式

```json
{
  "token": "token:0e360c63a3",
  "error": "Get \"https://chatgpt.com/backend-api/me\": dial tcp ...: connection timed out"
}
```

`token` 是**脱敏**的（`token:` + 前 10 位哈希），不是完整 token。

### 边界行为（实测）

| 输入 | 结果 |
|:--|:--|
| 空 body `{}` | `400` `{"detail":{"error":"tokens is required"}}` |
| 非 JSON | `400` `{"detail":{"error":"invalid JSON body"}}` |
| `tokens` 不是数组 | `400`，`detail` 是**数组**形式的校验错误（见下） |
| `accounts` 条目缺 `access_token` | **静默丢弃**，`added: 0`，`errors: []` |
| 请求体 > 8 MiB | `413` `{"detail":{"error":"request body too large: limit is 8388608 bytes"}}` |

`tokens` 类型错误时的 `detail` 是数组（对齐 FastAPI/pydantic）：

```json
{"detail":[{"input":"notarray","loc":["body","tokens"],"msg":"Input should be a valid list","type":"list_type"}]}
```

---

## 2.5 查看详情：`GET /api/accounts/detail`

```
GET /api/accounts/detail?access_token=<urlencoded>
```

### 查询参数

| 参数 | 类型 | 必填 | 说明 |
|:--|:--|:--|:--|
| `access_token` | string | ✅ | 要查的账号。**必须 URL 编码** |

**必须 `encodeURIComponent`**：token 可能是 JWT（含 `.`），也可能是 session
形态（含 `+` `/` `=`）。不转义时 `+` 会被解析成空格、`&` 会截断参数 ——
症状是「账号明明在池子里，详情却 404」。

### 返回

```json
{ "item": { /* 完整账号对象 + 派生字段 */ } }
```

与列表接口的**关键差异**：详情返回**凭据原文**（`password`、`totp_secret`、
`refresh_token` 等），列表接口只给 `*_set` 布尔值。

设计理由：列表是批量读，一次泄漏等于全池接管；详情是显式的单条查询，
功能本身就是「看这个账号的密码和 2FA 种子」，不给原文就没有意义。

**两个接口都需要 admin 权限**，无 auth 或错误 auth 一律 401。

### 派生字段（详情独有）

| 字段 | 类型 | 说明 |
|:--|:--|:--|
| `access_token_expires_in` | int | AT 剩余秒数。**-1 = 未知**（该 token 不是 JWT） |
| `access_token_expires_at` | string | AT 过期绝对时间（RFC3339）。未知时为空串 |
| `proxy_source` | string | `account` / `global` / `direct` |
| `proxy_effective` | string | 实际生效的代理地址；`direct` 时为空 |
| `has_password` | bool | 是否配了密码 |
| `has_totp_secret` | bool | 是否配了 2FA 种子 |
| `has_refresh_token` | bool | 是否有 RT |
| `has_id_token` | bool | 是否有 ID token |
| `has_session_token` | bool | 是否有 session token |

**`-1` 与 `0` 的语义必须分开**：`0` 会和「已过期」混淆，前端会显示
「已过期」误导运维去重登一个其实好用的号。所以非 JWT 用 `-1`（未知）。

### 错误

| 情况 | 状态码 | 响应 |
|:--|:--|:--|
| 缺 `access_token` 参数 | 400 | `{"detail":{"error":"access_token is required"}}` |
| 账号不存在 | 404 | `{"detail":{"error":"account not found"}}` |
| 无 / 错误 auth | 401 | `{"detail":{"error":"密钥无效或已失效，请重新登录"}}` |

### 示例

```bash
TOKEN='eyJhbGciOiJSUzI1Ni...'
curl -sS --get "$BASE/api/accounts/detail" \
  -H "$AUTH" \
  --data-urlencode "access_token=$TOKEN"
```

---

## 2.6 `account_id`（稳定标识）

每个账号有一个 **UUID 形态的稳定标识**，从创建起终身不变。

```json
{ "account_id": "838823ef-f8f8-5b75-8ad1-e7221d78a7d6" }
```

### 为什么需要它

`access_token` **会变**。两条路径都会换掉它：

| 路径 | 触发时机 |
|:--|:--|
| RT 换 AT（keepalive） | 每 3 天一次 |
| 密码重登 | token 失效后自动恢复 |

拿 `access_token` 当引用键的后果：

- 前端 React key 变化 → 该行 DOM 被重建，**选中态与展开态丢失**
- SQL 后端会把旧行 `DELETE`、新行 `INSERT`，自增 `id` 随之改变（实测 `id=1` → `id=3`）
- 外部系统（报表、审计日志、注册机数据库）记下的引用在轮换后失效

`account_id` 不受这些影响。

### 生成规则

**你不必传它** —— 后端会自动处理：

1. **新账号**：创建时生成 UUIDv5（确定性，非随机）
2. **已带 `account_id` 的记录**：原样保留
3. **历史账号**（升级前导入的）：按种子**确定性派生**

派生用的种子优先级：

```
user_id  >  email  >  access_token
```

**为什么是确定性派生而不是随机生成**：随机的话每次服务重启都会给同一账号一个新
ID —— 那就等于没有稳定标识。确定性派生保证即使某次保存失败、或你从旧备份恢复
`accounts.json`，ID 也不会变。已实测：260 个账号重启前后 ID 全部一致。

### 用于别处

**详情接口可以按 `account_id` 查**，不必再用 token：

```bash
curl -sS --get "$BASE/api/accounts/detail" \
  -H "$AUTH" \
  --data-urlencode "account_id=838823ef-f8f8-5b75-8ad1-e7221d78a7d6"
```

三个参数名都接受，优先级 `access_token` → `account_id` → `id`（`id` 是简写）。

### 建议

| 场景 | 用哪个 |
|:--|:--|
| 外部系统记录账号 | **`account_id`** |
| 前端行标识 / 选中态 | **`account_id`** |
| 书签、日志、审计 | **`account_id`** |
| 调用刷新/删除/导出接口 | `access_token`（这些接口按 token 工作） |

---

## 3. 编辑：`POST /api/accounts/update`

### 请求体

| 字段 | 类型 | 必填 | 说明 |
|:--|:--|:--|:--|
| `access_token` | string | ✅ | 要改的账号 |
| `type` | string | | 套餐类型 |
| `status` | string | | 账号状态 |
| `quota` | int | | 图片额度 |
| `proxy` | string | | 出口代理，`""` = 清空回退全局 |
| `password` | string | | 账号密码（用于失效后自动重登） |
| `totp_secret` | string | | Base32 TOTP 密钥，见 [6.5](#65-两步验证2fa--totp) |
| `email` | string | | 账号邮箱 |

**只认这 7 个键**，其余字段传了会被忽略。只改传了的字段，没传的保持不变。

```json
{ "access_token": "eyJhbGci...", "proxy": "socks5h://u:p@10.0.0.2:1080" }
```

### 返回

```json
{ "item": { /* 改后的账号 */ }, "items": [ /* 全部账号 */ ] }
```

### 错误

| 情况 | 状态码 | 响应 |
|:--|:--|:--|
| 缺 `access_token` | 400 | `{"detail":{"error":"access_token is required"}}` |
| 没有任何可改字段 | 400 | `{"detail":{"error":"还没有检测到改动，请修改后再保存"}}` |
| 账号不存在 | 404 | `{"detail":{"error":"account not found"}}` |

---

## 4. 批量代理：`POST /api/accounts/proxy`

两种形态，**可同时使用**（`proxies` 先写入，随后统一 `proxy` 覆盖）。

### 形态 A —— 统一代理

| 字段 | 类型 | 说明 |
|:--|:--|:--|
| `access_tokens` | `string[]` | 目标账号 |
| `proxy` | string | 统一代理；`""` = 全部清空 |

```json
{
  "access_tokens": ["at-1", "at-2", "at-3"],
  "proxy": "socks5h://user:pass@10.0.0.1:1080"
}
```

### 形态 B —— 逐个映射

| 字段 | 类型 | 说明 |
|:--|:--|:--|
| `proxies` | `object<string,string>` | key 是 access_token，value 是代理；`""` = 清空该账号 |

```json
{
  "proxies": {
    "at-1": "socks5h://user:pass@10.0.0.1:1080",
    "at-2": "http://10.0.0.2:8080",
    "at-3": ""
  }
}
```

### 返回

```json
{ "updated": 3, "missing": [], "items": [ /* 全部账号 */ ] }
```

| 字段 | 类型 | 说明 |
|:--|:--|:--|
| `updated` | int | 成功更新的数量 |
| `missing` | `string[]` | 找不到的 access_token |
| `items` | array | 全部账号对象 |

### 错误

| 情况 | 状态码 | 响应 |
|:--|:--|:--|
| 既没有 `proxies` 也没有 `access_tokens`+`proxy` | 400 | `{"detail":{"error":"需要 access_tokens + proxy，或 proxies 映射"}}` |

---

## 5. 代理字符串格式

```
<scheme>://[<user>:<password>@]<host>:<port>
```

### 支持的 scheme

| scheme | DNS 解析位置 | 说明 |
|:--|:--|:--|
| `socks5h://` | **代理侧** | 推荐。目标域名不暴露给本地 DNS |
| `socks5://` | 本地 | 本地解析后把 IP 发给代理 |
| `http://` | 本地 | HTTP CONNECT 隧道 |
| `https://` | 本地 | TLS 到代理本身 |

### 样例

```
socks5h://user:pass@10.0.0.1:1080
socks5://10.0.0.1:1080
http://user:pass@proxy.example.com:8080
https://user:pass@proxy.example.com:8443
```

### 注意

- **必须带 scheme**。`10.0.0.1:1080` 会被当成非法 URL
- 用户名/密码含特殊字符时要 URL 编码（`@` → `%40`，`:` → `%3A`）
- 空白字符串（`"  "`）等同于未配置，自动回退全局代理
- 代理不可用时该账号所有上游请求失败，表现为 `status: "异常"` 或 502

### 生效范围

| 出站请求 | 用账号代理 |
|:--|:--|
| 账号信息刷新（`/me`、`conversation/init`、`accounts/check`） | ✅ |
| 文本对话（`/v1/chat/completions`、`/v1/responses`、`/v1/messages`） | ✅ |
| 图片生成 / 编辑 | ✅ |
| 搜索 | ✅ |
| PPT / PSD 生成 | ✅ |
| 模型目录（`/v1/models`） | ✅ |
| **密码重登** | ❌ 走全局代理 |

密码重登走全局的原因：登录时手上只有邮箱和密码，还没拿到 token，
无法反查是哪个账号的代理。

### 优先级

```
账号 proxy  →  config.json 的 "proxy"  →  直连
```

实测四种组合：

| 账号 proxy | 全局 proxy | 实际使用 |
|:--|:--|:--|
| `socks5h://own...` | `http://global...` | `socks5h://own...` |
| 空 | `http://global...` | `http://global...` |
| 空白 `"  "` | `http://global...` | `http://global...` |
| 空 | 空 | 直连 |

---

## 6. 枚举值

### `status` —— 账号状态

**写入时不校验**（实测传 `bogus` 也会原样存储）。但只有下面 4 个值有实际语义：

| 值 | 含义 | watcher 会刷新吗 |
|:--|:--|:--|
| `正常` | 可用 | ✅ |
| `限流` | 额度耗尽（`quota == 0`） | ✅ |
| `异常` | token 失效或多次失败 | ✅ 按 `invalid_count` 指数退避 |
| `禁用` | 人工停用 | ❌ |

刷新时 `status` 会被上游结果**覆盖**（`quota == 0` → `限流`，否则 `正常`），
所以手工设成 `正常` 并不会让一个失效账号真的恢复 —— 那需要重新登录。

**建议**：只用上表 4 个值。传其它值会让账号不在任何刷新列表里
（`ListLimitedTokens` / `ListNormalTokens` 等都按状态精确匹配）。

### `type` —— 套餐类型

**存储时保留原值**（实测：传 `plus` 存 `plus`，传 `Team` 存 `Team`），
只有在**路由与模型目录**比较时才做别名归一化 —— 也就是说 `team`、`Team`、
`business` 在选号时被视为同一档，但列表里显示的是你传进来的字符串。

| 输入 | 存储 | 选号时等价于 |
|:--|:--|:--|
| `free` | `free` | free |
| `plus` | `plus` | Plus |
| `pro` | `pro` | Pro |
| `prolite` | `prolite` | ProLite |
| `team` | `team` | Team |
| `business` | `business` | Team |
| `enterprise` | `enterprise` | Enterprise |
| 其它任意值 | 原样 | 按原值比较 |
| 空 | `free` | free |

归一化规则（用于比较）：小写化 → `-` 和空格替换为 `_` → 去下划线后查别名表。

**建议**：按上表右列的标准写法传（`Plus`、`Pro`、`Team`），显示更一致。

### `source_type` —— 账号来源

| 值 | 含义 |
|:--|:--|
| `web` | 网页 token 导入（默认） |
| `password` | 邮箱密码 + TOTP 登录获得 |
| `codex` | Codex 来源 |

---

## 6.5 两步验证（2FA / TOTP）

账号若开了两步验证，**自动重登必须拿到 `totp_secret`**，否则该账号失效后无法自动恢复。
这是注册机最容易漏的一个字段。

### 支持与不支持的 2FA 类型

| 上游挑战类型 | 本服务行为 | 需要账号带什么 |
|:--|:--|:--|
| 无 2FA | 直接换 token | 什么都不用 |
| **TOTP（认证器 App）** | ✅ 自动生成验证码并提交 | `totp_secret` |
| **邮箱验证码** | ❌ 无法自动完成，报 `need_verification_code` | — |
| 其他挑战 | ❌ 报 `no_auth_code`，附 `page.type` | — |

也就是说：**只支持 TOTP**。短信、邮箱验证码、硬件密钥都不行 ——
这些账号导入后一旦 RT 失效，只能人工重新登录。

### `totp_secret` 的格式

标准 **Base32** 字符串，即认证器 App「手动输入」时看到的密钥。

```
JBSWY3DPEHPK3PXP
```

生成算法：RFC 6238 TOTP，**6 位数字**，**30 秒**周期（与 Google Authenticator 默认一致）。

### 解析容错（实测）

下列写法**全部等价**，都能正确生成同一个验证码：

| 写法 | 说明 |
|:--|:--|
| `JBSWY3DPEHPK3PXP` | 标准大写无填充 |
| `jbswy3dpehpk3pxp` | 小写 |
| `JBSW Y3DP EHPK 3PXP` | 带空格 |
| `JBSWY3DPEHPK3PXP====` | 带 `=` 填充 |
| `jbsw-y3dp-ehpk-3pxp` | 带连字符 |
| `  JBSWY3DPEHPK3PXP  ` | 首尾空白 |

处理规则：转大写 → 去空格 → 去连字符 → 去尾部 `=` → Base32 解码。

**会被拒绝**：空串、纯空白、非 Base32 字符（如 `not-base32!!!`）。
报错 `totp_secret_invalid`。

### 从哪些地方取这个值

| 来源 | 拿法 |
|:--|:--|
| 注册机自己生成 | 注册时自己生成的 Base32 串，直接落库 |
| 认证器 App 手动输入 | App 里显示的密钥（去掉空格） |
| `otpauth://` 二维码 | 取 `secret=` 参数的值 |

`otpauth://totp/...?secret=JBSWY3DPEHPK3PXP&issuer=...` → 要的是 `JBSWY3DPEHPK3PXP`。

### 没有 `totp_secret` 会怎样

| 时机 | 表现 |
|:--|:--|
| 导入时 | 正常导入，不报错 |
| 首次刷新 | 正常（AT 还有效，不需要 2FA） |
| **AT 失效后自动重登** | 失败，`need_totp`：「该账号开启了 TOTP 两步验证，但账号里没有 totp_secret」 |
| 结果 | 账号停在「异常」，**无法自动恢复** |

所以：**只要账号开了 2FA，导入时就必须带 `totp_secret`**，否则这个账号是一次性的
（AT 用完就废）。

### 只有密码、没有 TOTP 的账号

上游若没开 2FA，`password` 就够了，`totp_secret` 留空即可。
这类账号在 `amr` 里不含 `otp` 项。

### 上传样例

```json
{
  "accounts": [
    {
      "access_token": "eyJhbGci...",
      "refresh_token": "rt.1...",
      "email": "user@example.com",
      "password": "P@ssw0rd",
      "totp_secret": "JBSWY3DPEHPK3PXP",
      "proxy": "socks5h://u:p@10.0.0.1:1080"
    }
  ]
}
```

### 补 `totp_secret` 到已有账号

已导入但漏了 TOTP 的账号，直接用 `POST /api/accounts/update` 补：

```json
{ "access_token": "eyJhbGci...", "totp_secret": "JBSWY3DPEHPK3PXP" }
```

`password` 与 `email` 同样可以这样补。三个字段都在 `update` 的可改白名单里：

```
type  status  quota  proxy  password  totp_secret  email
```

**只改传了的字段** —— 没传的保持不变，不会被覆盖成空。

---

## 7. 错误响应

### 单个对象（最常见）

```json
{"detail": {"error": "tokens is required"}}
```

### 数组（类型校验失败，对齐 FastAPI/pydantic）

```json
{"detail": [{"input": "notarray", "loc": ["body","tokens"], "msg": "Input should be a valid list", "type": "list_type"}]}
```

### 状态码一览

| 状态码 | 场景 |
|:--|:--|
| `200` | 成功（**注意：上传部分失败也是 200**，失败明细在 `errors` 里） |
| `400` | 请求格式错误、缺必填字段、无可改字段 |
| `401` | AUTH_KEY 无效 → `{"detail":{"error":"密钥无效或已失效，请重新登录"}}` |
| `404` | 账号不存在 |
| `413` | 请求体超 8 MiB → `{"detail":{"error":"request body too large: limit is 8388608 bytes"}}` |

**重要**：`POST /api/accounts` 即使有账号刷新失败也返回 `200`，
要判断是否真的成功，看 `errors` 数组是否为空。

---

## 8. 限制与上限

| 项 | 值 | 说明 |
|:--|:--|:--|
| 请求体上限 | **8 MiB** | 超限 `413`。`/api/accounts` 不在图片端点白名单里 |
| 单批账号数 | 约 **2400** | 实测约 3.5 KB/账号。建议每批 ≤1000 留余量 |
| `refresh=1` 阻塞时间 | 每账号约 1–3 s | **串行** 3 次上游请求（me → conversation/init → accounts/check）；代理慢时更久 |
| 图片端点上限 | 16 MiB | `/v1/images/*`、`/v1/ppt/*`、`/v1/psd/*` 等 |

### 为什么要分批

8 MiB ÷ 3.5 KB ≈ 2400 个账号。但：

- 带 `password` + `totp_secret` 的账号对象更大（约 4–5 KB）
- JSON 转义会让长 token 略膨胀

**建议每批 1000 个**，留足余量。超限时会被 413 拒绝，整批失败。
