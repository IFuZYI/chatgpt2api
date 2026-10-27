# AGENTS.md — 本项目的作业规范

给在本仓库工作的 AI agent（与人类协作者）的硬规则。每条都来自实际踩过的坑，
附来源出处。

---

## 交付前的验证（必须）

### 1. 前端改动必须重建 embed 产物并重启用户实际在用的服务

前端是**编译进 Go 二进制**的（`//go:embed all:web_dist`）。只改 `web/src/**`
不会影响正在运行的服务 —— 用户看到的仍是旧页面。

完整流程：

```bash
cd web && npm run build        # 内部走 scripts/build.mjs，会注入 VERSION
cd .. && rm -rf internal/web/web_dist && cp -r web/out internal/web/web_dist
git checkout internal/web/web_dist/.gitkeep     # 该文件必须保留
go build -o chatgpt2api ./cmd/server
# 然后重启用户实际访问的那个服务
```

> `npm run build` 现在由 `web/scripts/build.mjs` 包装：它读仓库根的
> `VERSION` 注入 `NEXT_PUBLIC_APP_VERSION`。直接调 `next build` 会让
> 界面上的版本号显示成 `0.0.0`（2.2.3 修复）。

**在临时实例（`--base-dir /tmp/xxx`）上验证不等于交付完成** ——
它证明代码正确，不证明用户侧生效。两件事都要做。

排查「我改的东西没出现」时，先对比时间戳：

```bash
PID=$(ss -lntp | grep ":8080" | grep -oP 'pid=\K[0-9]+' | head -1)
ls -l /proc/$PID/exe                      # 二进制构建时间
ls -la internal/web/web_dist/index.html   # 前端产物时间
```

来源：`.learnings/LEARNINGS.md#LRN-20261026-001`

### 2. 提交信息必须与 diff 核对

写 commit message 前跑一次 `git show --stat`，确认 message 里提到的**每个文件**
都真的在改动清单里。

批量文本替换（Python/sed）之后尤其要小心：一步 `assert` 失败可能让**整个文件
未写入**，而脚本输出看似成功。

```bash
grep -n '<新符号>' <文件>   # 确认真的写进去了
go build ./...              # 确认能编译
```

来源：`.learnings/LEARNINGS.md#LRN-20261026-005`、`.learnings/ERRORS.md#ERR-20261026-001`

---

## 安全

### 3. 凭据脱敏必须覆盖所有返回账号的端点

`ListAccounts()` 返回的是号池**原始记录**（含凭据原文）。任何直接返回它的
新端点都会静默泄漏。

- 脱敏范围是 **5 个字段**：`password`、`totp_secret`、`refresh_token`、
  `id_token`、`session_token`
- 脱敏**不得改写入参** —— 就地 `delete` 会删掉号池内存里的密码，导致重登失败
- 新增「返回账号列表」的端点时，必须确认脱敏并补测试

对照测试：`internal/api/account_mask_test.go`。

来源：`.learnings/LEARNINGS.md#LRN-20261026-003`

### 4. 标识派生必须是确定性纯函数

给已有数据回填标识时，**不要用随机值**（`uuid.New()`）—— 那会让每次服务重启
给同一记录一个新 ID，等于没有稳定标识。

本仓库用 `uuid.NewSHA1(namespace, seed)`，种子取**第一个非空字段**
（`user_id > email > access_token`）。

`accountIDNamespace` 是**固定常量，改了会让所有历史账号派生出新 ID**。

### 关键：纯函数无法同时满足「稳定」与「唯一」

这两个需求**本质冲突** —— 唯一性要求把池内唯一的 `access_token` 纳入种子，
稳定性则要求它**不**纳入（否则轮换会派生新 ID）。实测踩过：

- 把 token 纳入种子 → 轮换稳定性崩（既有测试立刻失败）
- 只用一个种子字段 → 两个账号共享 `user_id`/`email` 时**派生出相同 ID**，
  且按 ID 查询会返回**不确定的账号**（map 迭代顺序随机）

**正确做法是把两件事分开**：

| 关注点 | 位置 | 职责 |
|:--|:--|:--|
| 派生 | `ensureAccountID`（纯函数） | 只保证「同种子同结果」 |
| 唯一性 | `disambiguateAccountIDs`（插入点） | 看到全局，消歧并优先保留已持久化 ID |

插入点实现有个坑：`persisted` 集合必须在插入循环**之前**快照。
若用「id 非空」判断，`normalizeAccount` 已给每个账号填过 id，
会把本轮刚派生的也当成已持久化，导致冲突账号拿不到消歧。

对照测试：`internal/service/account_id_test.go`。

来源：`.learnings/LEARNINGS.md#LRN-20261026-004`、`.learnings/LEARNINGS.md#LRN-20261026-006`

---

## 写文档

### 5. 字段说明必须覆盖三要素

只列字段名不算文档。每个字段要写清：

1. **哪些端点**可写/可读
2. **取值范围或格式**限制（例如 2FA 只支持 TOTP，不支持邮箱/短信）
3. **缺失它的后果**（报错？静默失败？还是某功能永远不生效）

验证方式是**照着文档走一遍**，不是照着代码列字段。

来源：`.learnings/LEARNINGS.md#LRN-20261026-002`

---

## 本仓库的操作要点

### 测试

```bash
go test ./...                                    # 后端（650+ 测试）
cd web && npm test && npm run typecheck          # 前端（node --test）
go test -race ./internal/service/ ./internal/api/
```

**时区敏感**：部分测试涉及时间字段解析。CI 跑
`UTC / Asia/Shanghai / America/Los_Angeles` 三个时区 —— 只跑 UTC 会漏掉
时区类缺陷（实测：撤销某修复后 UTC 全绿，另两个时区才失败）。

### 清理测试实例

用端口定位 PID，**不要用 `pkill -f <pattern>`** —— pattern 会匹配到执行这条
命令的 shell 自身。

```bash
PID=$(ss -lntp | grep ":18099" | grep -oP 'pid=\K[0-9]+' | head -1)
[ -n "$PID" ] && kill $PID
```

来源：`.learnings/ERRORS.md#ERR-20261026-003`

### 测试包名

`internal/api/` 下同时存在两种测试包：

- `package api_test`（外部）—— `newTestDeps` 等 helper 在这里
- `package api`（内部）—— 需要访问未导出符号时用

写新测试前先 `head -2` 看邻居文件用哪个，别混用。

来源：`.learnings/ERRORS.md#ERR-20261026-002`

---

## 状态机与并发（新增于 2.2.3 审计后）

### 6. 状态不变量必须加在**每个写入点**

只加在「判断处」等于没加：`RemoveInvalidToken` 曾被 `RecordInvalidTokenSeen`
的返回值驱动，而它自己没有禁用保护 —— 一次日常的「刷新全池」就能把禁用号
降级甚至删除。

- 加完保护后，用 `grep 'status.*=.*Status'` 把所有写入点列出来逐个确认。
- 测试要用**真实触发路径**的种子数据：首版测试用新账号做种子，落进了
  「新号宽限」分支而**假通过**。
- 状态可用性判定用**白名单**（`isSchedulableStatus`），不要用黑名单 ——
  黑名单会让任何未识别的值（外部导入的 `banned`/`expired`）被当作可用号。

来源：`.learnings/LEARNINGS.md#LRN-20261027-009`

### 7. 去重键必须是「实体身份」；释放用 defer 且传 begin 返回的键

`beginRelogin(token)` 曾用原始 token 当键 —— token 轮换建立别名后，
同一账号有两个字符串，两条路径各持一个就能并发双登录。

- 先 `resolveTokenLocked` 解析成规范身份再当键。
- `end*` 必须用 `begin*` **返回的键**（期间可能轮换），并用 `defer` 释放
  （手动调用在 `continue`/panic 路径会漏，泄漏后该账号永久无法处理）。

来源：`.learnings/LEARNINGS.md#LRN-20261027-010`

### 8. 凭据脱敏放在**唯一出境点**（落盘/序列化）

`redactURLCredentials` 早就存在，但只被「测试代理」按钮调用 ——
代理密码经 `url.Parse` 的错误文本进入 `accounts.json`，再由
`GET /api/accounts` 返回、在详情弹窗里可复制。

- 脱敏不放在错误产生处（第三方库的错误文本你控制不了它带什么），
  放在落盘/序列化这一唯一出境点。
- 已有脱敏工具要查**调用点数量**：没被调用等于没有。

来源：`.learnings/LEARNINGS.md#LRN-20261027-011`

### 9. 「保守默认」按可观测性判断，不按是否改变状态

把未知错误码当 transient（注释写"保守，不误杀"）的实测后果是：账号永远卡在
异常、每轮白跑、无人知道原因 —— 那不是保守，是把故障藏起来。

- 改成激进默认（未知即终态）时必须同时给：**留痕字段**、
  **人工恢复出口**、**二次确认**（防一轮上游故障批量误杀）。

来源：`.learnings/LEARNINGS.md#LRN-20261027-012`

### 10. CI 必须覆盖前端与容器

2.2.1/2.2.2 的「刷新页面崩溃」能漏出去，是因为 CI 只跑 Go ——
`items` 字段被移除后没人验证前端编译产物。

- `test.yml` 现在有 `web` job（npm test / typecheck / build）与
  `docker-smoke` job（构建镜像 + `/health` 探活）。改前端或 Dockerfile
  时必须保持这两个 job 绿色。

来源：`.learnings/LEARNINGS.md#LRN-20261027-013`
