# 账号生命周期：刷新 / 异常 / 重登 / 禁用

本文描述号池中一个账号从「正常」到「禁用」的完整状态机、每个状态的
自动维护动作、失败分类规则，以及为什么这样设计。它是
[ADR-0014](adr/0014-abnormal-account-recovery-and-disable.md) 的配套说明。

> 相关代码：`internal/service/account_invalid.go`、`account_relogin.go`、
> `account.go`（选择器）、`watcher.go`、`password_login.go`。

---

## 1. 四个状态

| 状态 | 含义 | 是否参与调度 | 是否自动维护 |
|:--|:--|:--|:--|
| 正常 | 可用 | 是（图片要求 `quota>0`） | 是（刷新 + keepalive） |
| 限流 | 上游限流，有 `restore_at` | 图片否 / 文本是 | 是 |
| 异常 | token 判定失效，暂停调度，等待恢复 | 否 | 是（退避重试 + 重登） |
| 禁用 | 自动路径已判死，**终态** | 否 | **否** |

「异常」与「禁用」的分界是**自动路径还能不能救**：

- **异常** = 还有救（补凭据、等上游恢复、密码重登）
- **禁用** = 自动路径已确认救不回来，只能人工处理

「禁用」**不等价于封号**。封号只是它最常见的成因；密码错、缺凭据、
缺 2FA 密钥（自动路径永远无法修复）同样会落到禁用。

---

## 2. 状态机

```
                    ┌─────────────────────────────────────────┐
                    │                                          │
   [导入] ──▶ 正常 ──┬─ quota→0 ──▶ 限流 ── restore_at 到期 ──┘
                    │                 （watcher 刷新解除）
                    │
                    └─ 401 / 403(来自 /me) ──▶ 失效观察
                                                  │
                        ┌─────────────────────────┼──────────────────────┐
                        │                         │                      │
                  ① 新号宽限(<10min)        ② 短窗去抖(<30s)      ③ 确认失效
                        │                         │                      │
                     不变                      不变              标异常 + quota 0
                                                                        │
                                              ┌─────────────────────────┘
                                              │
                                   自动维护（退避重试 / 密码重登）
                                              │
              ┌───────────────────────────────┼──────────────────────────┐
              │                               │                          │
        重登成功                     不可恢复失败                 环境类失败
              │                               │                          │
        正常 + 清零                      禁用（终态）              保持异常，继续退避
        invalid_count / 失败标记        + disabled_reason          （地区/限流/网络）
```

**关键不变量**：

1. 禁用是**吸收态**。所有写入点都有保护，逐个列出（漏一个等于没保护）：
   - `applyRefreshedAccount`：刷新成功不合并上游 status
   - `RecordInvalidTokenSeen`：401 命中不降级
   - `RemoveInvalidToken`：**入口直接跳过**（它的调用方由上面的返回值驱动，
     原本这里无保护 → 批量刷新会把禁用号降级或删除）
   - `ApplyReloginTokens`：登录期间被禁用的不撤销（见 §5.5）
   - `MarkImageResult`：在途图片任务只允许在「正常 ↔ 限流」间转换
   - `addAccountPayloads`：重新导入不覆盖已有禁用记录
2. 禁用号**不进任何自动维护列表**（`isAutoMaintainable` 统一判定）。
3. 恢复成功必须**清零**失效计数与所有派生标记。
4. **状态判定用白名单**：`isSchedulableStatus` 只认「正常」「限流」，
   未知状态一律不可调度（黑名单写法会让导入的 `banned`/`expired` 放行）。

---

## 3. 失效观察：三道防线

`RecordInvalidTokenSeen` 每次失效 +1，然后按三道防线决定动作
（`invalidTokenDecisionLocked`）：

| 防线 | 条件 | 动作 |
|:--|:--|:--|
| ① 新号宽限 | `created_at` < 10 分钟 | 不变（会话可能还没就绪） |
| ② 短窗去抖 | 距上次失效 < 30 秒 | 不变（同一轮重复命中） |
| ③ 确认失效 | 其余 | **标异常 + quota 归零**；`invalid_count>=2` 时允许删除 |

> **为什么③要标异常**：早期实现的 defer 语义是「不删」，但它连
> 「标异常」也一起跳过了，于是首次 401 的账号带着 `quota=0` 和 401
> 继续以「正常」参与调度 —— 文本链路会选到它，然后请求失败。
> defer 只应保护它真正想保护的那个动作（删除）。

删除门槛保持原语义（更新前 `invalid_count >= 2`，即第三次才允许删），
且仍受 `auto_remove_invalid_accounts` 开关约束。

---

## 4. 自动维护：两条独立路径

### 4.1 刷新（`FetchAccountInfo`）

只读元信息（email / quota / plan / restore_at），**从不换 token**。
成功时合并上游字段并**清零** `invalid_count` / `last_invalid_at`。

### 4.2 keepalive（RT → 新 AT+RT）

要求账号带 `refresh_token`，且锚点（`last_token_refresh_at`，回退到
AT 签发时间，再回退到 `created_at`）距今 **3 天**。成功时轮换三件套，
但**不改 status**（由刷新负责）。

### 4.3 异常号的重试节奏

```
delay = min(base × 2^(count-1), max)        base=5min  max=6h
实际等待 = delay/2 + rand(0, delay/2)        ← full jitter 下半区间
```

**抖动是必须的**。没有它时，同批失败的账号得到完全相同的退避时长，
在同一时刻重新到期，watcher 每轮只能处理前 N 个，其余下一轮再同时
到期 —— 整个池子被锁进同一个节拍。实测：168 个异常号，
watcher 每轮只挑出 1~19 个。

---

## 5. 重登（密码登录换 AT/RT）

### 5.1 触发点

| 触发 | 路径 | 是否自动 |
|:--|:--|:--|
| 刷新发现 401/403 | `RefreshAccounts` 收尾 | 自动（开关控制） |
| keepalive 的 RT 被拒 | `KeepaliveRefreshTokens` 收尾 | 自动（开关控制） |
| 面板「尝试恢复异常账号」 | `POST /api/accounts/re-login` | 手动 |

### 5.2 失败分类（`classifyReloginFailure`）

核心原则：**账号无论如何无法恢复 → 转「禁用」**（终态）。

| 错误码 | 归类 | 自动路径动作 |
|:--|:--|:--|
| `account_deactivated` | 不可恢复 | **禁用**（封号） |
| `invalid_password` / `invalid_username_or_password` | 不可恢复 | **禁用**（凭据已失效） |
| `no_password_in_account` / `no_email_in_account` | 不可恢复 | **禁用**（自动路径无从发起登录） |
| `need_totp` | 不可恢复 | **禁用**（本地缺 2FA 密钥，换不出新 AT/RT） |
| `need_verification_code` | 不可恢复 | **禁用**（自动路径读不了邮箱） |
| 未知码 | 不可恢复（默认） | **禁用** |
| `unsupported_country_region_territory` | 环境 | 保持异常，继续退避 |
| `rate_limit_exceeded` | 环境 | 保持异常，继续退避 |
| `invalid_state` / `no_auth_code` | 环境 | 保持异常，继续退避 |

**只有环境类失败保持「异常」**——它们不是账号本身的问题，禁用等于误杀。
判定采用"环境白名单"：只有明确可恢复的环境错误才留在异常，
其余（含未知码）一律按不可恢复处理。

**未知码按不可恢复处理的取舍**：早期实现把未知码当 transient（"保守"），
实测后果是账号永远卡在异常、无限重试且无人知道原因。现在改为禁用，
但 `last_relogin_code` 会落盘 —— 若发现良性码被误判，可据此定位并
补进环境白名单。

**手动路径永不禁用**：人工点按钮时不该因为一次失败就把账号判死。
手动路径仍会写 `last_relogin_code`，面板上能看到原因。

### 5.3 并发保护

- `beginRelogin` / `endRelogin`：同一账号同一时刻只允许一次密码登录。
  没有它时，刷新收尾与 keepalive 收尾会同时对同一账号发起登录，
  两套 AT/RT 互相覆盖，且对上游表现为「同一出口极短时间内两次完整登录」。
- `reloginMaxConcurrency = 2`：限制全局并发密码登录数。

### 5.4 重登并发保护

- `beginRelogin` / `endRelogin` 的键是**解析后的账号身份**
  （`resolveTokenLocked`），不是原始 token 字符串 —— token 轮换会建立别名，
  按字符串去重会让同一账号被并发登录两次。
- `endRelogin` 必须传 `beginRelogin` 返回的键，并用 `defer` 释放：
  轮换后重新解析会删错键（占用位永久残留），手动调用在 panic 路径会漏。

### 5.5 人工解禁与「登录期间被禁用」

`applyReloginTokens(oldToken, issued, wasDisabledBeforeLogin)` 用**登录前的
状态**区分两个相反的需求：

| 登录前状态 | 登录期间发生 | 结果 |
|:--|:--|:--|
| 禁用 | — | 成功 → 解禁（人工点「尝试恢复」的唯一出口） |
| 非禁用 | 被别人禁用 | 成功 → **保留禁用**，只更新凭据 |

不区分的话，人工禁用会被一次并发的成功回写静默抹掉，连
`disabled_reason` 一起消失，排查时毫无痕迹。

### 5.6 错误分类必须覆盖每个步骤

上游把 `account_deactivated` 报在 **mfa/verify**（实测：密码校验能过，
MFA 才回 403）。因此判定抽成 `accountDeactivatedSignals(code, message)`，
在 `passwordVerifyError` 与 `mfaVerifyError` 里都调用。

判定同时看 code 与 message —— 上游不同步骤给的 code 不一致，
但 message 里固定出现 `deleted or has been deactivated`。

---

## 6. 禁用：终态

### 6.1 唯一写入点

`disableAccount(token, reason, detail)`，落盘：

| 字段 | 说明 |
|:--|:--|
| `status` | `禁用` |
| `quota` | `0` |
| `disabled_reason` | 错误码（`account_deactivated` / `invalid_password` / …） |
| `disabled_at` | 判定时间 |
| `disabled_detail` | 截断到 300 字符的上游原文 |

已经是禁用的账号不会被重复改写（保留首次判定的原因与时间）。

### 6.2 隔离

`isAutoMaintainable(account)` 是唯一判定：`status == 禁用` 的账号
不参与任何自动维护。被排除在：`ListExpiringAccessTokens`、`ListRefreshTokenKeepaliveTokens`、
`ListAbnormalTokens`、`ListMaintainableTokens`（整池刷新）。

### 6.3 人工恢复（禁用的唯一解禁通道）

面板的「尝试恢复异常账号」接受 **异常 + 禁用**（前端 `RECOVERABLE_STATUSES`）。

这是刻意的配套设计：既然不可恢复的账号会转禁用，就必须保证禁用后仍能
人工救回 —— 否则补上 2FA 密钥或密码后，账号会永远出不来。恢复成功会
清空 `disabled_*`，并重置失效计数。

---

## 7. 代理绑定（出口一致性）

优先级：**账号代理 > 全局代理 > 直连**，由 `AccountService.ProxyFor(token)`
统一解析。**所有账号级出站都必须走同一个出口** —— 这是防 IP 切换风控的
唯一手段：一个配了账号代理的号，如果刷新走代理而重登走全局，
对上游就是一次 IP 跳变。

| 路径 | 出口来源 |
|:--|:--|
| 文本 / 图片 / 搜索 / 可编辑文件 / 账号信息 | `accounts.ProxyFor(token)` |
| **密码重登** | `accounts.ProxyFor(token)` → 传给 `PasswordLoginFunc` |
| **keepalive（RT 换 AT）** | `accounts.ProxyFor(token)` → 传给 `TokenRefreshFunc` |
| OAuth 授权码换 token | 全局代理（账号尚未入池，无法按账号解析） |
| 模型目录匿名兜底 | 全局代理（无账号可归属） |

**实现要点**：

1. 代理由 **service 层解析**后作为参数传给执行体。执行体（在 `cmd/server`）
   **不得**自己读 `cfg.Proxy()` —— 它拿不到账号身份，只能看到全局配置。
2. `newProxyHTTPClient` 必须区分协议：`http/https` 走
   `http.Transport.Proxy`，`socks5/socks5h` 必须走 `x/net/proxy` 的
   拨号器（`http.Transport.Proxy` 只认 http 系，遇到 socks 会报
   `unsupported protocol scheme`）。
3. `normalizeProxyURL` 统一处理 `socks://`、`socks5://`、
   `host:port:user:pass` 简写 —— 真实出站与「测试代理」按钮用同一套归一化，
   避免「测试通过、实际失败」。

**验证方式**：`TestAllAccountEgressPathsResolveSameProxy` 断言同一账号的
刷新、重登、keepalive 三条路径解析到同一个代理字符串；
`TestNewProxyHTTPClientSupportsSOCKS5` 断言 socks5 真的设置了
`DialContext`（而不是静默直连）。

---

## 8. 可观测字段

| 字段 | 用途 |
|:--|:--|
| `invalid_count` | 连续失效次数；驱动退避；**成功时清零** |
| `last_invalid_at` | 退避锚点；**成功时清空** |
| `last_refresh_error(_at)` | 最近一次刷新失败原因 |
| `last_relogin_error(_at)` / `last_relogin_code` | 最近一次重登失败（原实现完全不落盘） |
| `disabled_reason` / `disabled_at` / `disabled_detail` | 禁用判定依据 |
| `last_token_refresh_at` | keepalive 3 天锚点 |

---

## 9. 测试清单

| 文件 | 覆盖 |
|:--|:--|
| `account_lifecycle_test.go` | MFA 识别封号、自动/手动路径分叉、禁用隔离、刷新不复活、首次失效标异常 |
| `audit_regression_test.go` | 退避抖动与边界、成功清零计数、重登/刷新去重（含别名）、重登并发上限、panic 释放槽位、代理透传与脱敏、禁用不被复活 |
| `invalid_token_test.go` | 失效计数、403 来源区分、删除门槛 |
| `auto_relogin_test.go` | 开关语义、429 不触发、未接线不 panic |
