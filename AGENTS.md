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
cd web && npm run build
cd .. && rm -rf internal/web/web_dist && cp -r web/out internal/web/web_dist
git checkout internal/web/web_dist/.gitkeep     # 该文件必须保留
go build -o chatgpt2api ./cmd/server
# 然后重启用户实际访问的那个服务
```

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

本仓库用 `uuid.NewSHA1(namespace, seed)`，种子优先级
`user_id > email > access_token`（见 `internal/service/account.go`）。

`accountIDNamespace` 是**固定常量，改了会让所有历史账号派生出新 ID**。

新增此类回填逻辑时，必须有「重复加载得到相同值」的测试。

来源：`.learnings/LEARNINGS.md#LRN-20261026-004`

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
