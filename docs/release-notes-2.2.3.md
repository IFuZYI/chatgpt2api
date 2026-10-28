## ChatGPT2API 2.2.3

账号生命周期与整体稳定性的版本。重做了异常/重登/禁用状态机，统一账号出口代理，
并完成一轮覆盖八个维度的全仓审计。

### 账号生命周期与风控

- **异常账号自动收敛**：401 / `/backend-api/me` 的 403 会把账号摘出调度。
  封号、密码失效、缺凭据、缺 2FA 密钥等不可恢复情况转「禁用」；
  地区限制、限流、state 失效等环境问题保持「异常」并按指数退避重试。
- **MFA 阶段也识别封号**：实测上游在 `mfa/verify` 而不是密码校验步骤返回
  `account_deactivated`。此前它被降级成通用 403，账号会永远卡在异常并无限重试。
- **禁用成为真正终态**：刷新、keepalive、自动重登、批量刷新、在途图片回写、
  重新导入都不能复活或删除禁用号。人工「尝试恢复」是唯一解禁通道。
- **未知错误码需二次确认**：连续两次看到同一个未知码才禁用，防止一轮上游
  故障把整池健康账号打成需要人工解开的终态。

### 出口 IP 一致性

- 文本、图片、搜索、账号刷新、**密码重登**、**RT 续期**全部统一走
  `账号代理 > 全局代理 > 直连`。此前重登用全局代理、RT 续期用裸 client，
  同一账号在不同路径上从不同 IP 出去 —— 这本身就是一次 IP 跳变。
- socks5/socks5h 使用真正的 SOCKS5 拨号器（`http.Transport.Proxy` 只认 http 系），
  并保持 HTTP/2 开启。
- 代理不可达时直接失败，**不回退直连**。

### 稳定性

- 进度清理、图片清理、备份三个后台循环补齐 panic 恢复（Go 中未捕获的
  goroutine panic 会终止整个进程）；清理/备份原本静默吞掉 panic，现在会记日志。
- 后台图片任务增加 30 分钟兜底超时：上游 TCP 挂起时不再永久占用账号槽位。
- 生图页轮询在组件卸载后停止（此前会持续每 2 秒请求）。
- JSON 存储补上父目录 fsync：断电时不再可能「旧文件没了、新文件没落盘」。

### 安全

- **代理凭据不再进入错误文本**：所有落盘点统一脱敏，密码不会出现在
  `accounts.json` 或可复制的详情字段里。
- 未鉴权的 `/health?format=json` 不再返回服务器绝对路径。
- 凭据脱敏覆盖 5 个字段；路径穿越（含编码变体、符号链接）已实测拦截。
- 升级 Next.js 16.2.3 → 16.4.3，`npm audit` 归零。

### 性能

| 路径 | 优化前 | 优化后 |
|:--|:--|:--|
| 文本选号（300 账号） | 43.8 µs / 5400 B / 1 alloc | 39.9 µs / 2 B / **0 alloc** |
| 图片轮转 | 67.2 µs / 12508 B / 5 alloc | 60.5 µs / 9819 B / 4 alloc |
| 图片取号（并行） | 80.3 µs（比串行还慢） | **55.9 µs（−30%）** |

### 可观测性

- 新增 `GET /api/metrics`（admin 鉴权）：goroutine 数、堆大小、GC 次数、
  运行时长、号池统计。
- 长稳实测：并发峰值 heap 75 MB / goroutine 32 / fd 17，
  2 分钟后全部回落基线（4 MB / 9 / 6）——**无泄漏**。

### 图片清理

- 保留期单位由「天」改为「分钟」，默认 1440。旧键 `image_retention_days`
  自动换算（`天数 × 1440`）并删除，保留时长不变。
- 清理判据从「路径里的日期」改为「文件修改时间」：原实现下保留期小于 1 天
  时会把当天所有图判为过期。

### 文档与部署

- **修复源码运行流程**：全新 clone 按原文档 `go build` 能编译通过，但二进制里
  没有前端，`/` 与 `/accounts` 全部 404。README（中/英）与部署文档已改为
  显式两步（先构建前端 → 再编译后端）。
- 本地 `npm run build` 现在也注入版本号（此前绕开 Dockerfile 会显示 `0.0.0`）。
- 新增 `docs/performance.md`、`docs/account-lifecycle.md`、ADR-0014。
- `config.example.json` 的 `auto_remove_invalid_accounts` 由 `true` 改为 `false`：
  照抄模板意味着账号被判不可恢复时直接删号。
- CI 新增前端（test/typecheck/build）与 Docker（构建 + `/health` 探活）任务。
- **`Dockerfile` 补上 `HEALTHCHECK`**：此前 compose 配了探活、唯独镜像本身没有，
  直接 `docker run` 时进程卡死也不会被标记为 unhealthy。
- **`--init-proxy-config` 现在尊重 `--base-dir`**：此前只认
  `CHATGPT2API_CONFIG_FILE`，否则硬编码 `/app/config.json`。
- **本地发布脚本 `scripts/build-release.sh`**：与 CI 同矩阵构建 6 平台产物，
  并拒绝在脏工作区上构建（Go 会把 VCS 信息编进二进制，脏工作区产出的包
  会自称 `dirty` 且字节不可复现）。**脚本自己构建前端**——前端是 embed 进
  二进制的，而 `internal/web/web_dist/` 在仓库里只有 `.gitkeep` 占位，
  少了这步会产出访问 `/` 与 `/accounts` **全 404** 的二进制。
- **构建可复现**：Next.js 默认的随机 build ID 会让嵌入前端的二进制每次
  构建都不同（实测 6/6 都不一样）；改为「版本 + 提交号」的
  `generateBuildId` 后，六平台产物两次构建字节一致。
- **发布包含多架构镜像**：`docker buildx` 构建 `linux/amd64` +
  `linux/arm64` 的 OCI 归档（与 `docker-publish.yml` 推送到 ghcr.io 的内容
  相同），无需 QEMU —— Dockerfile 靠交叉编译。

### 升级注意

- `image_retention_days` 自动迁移为 `image_retention_minutes`，无需手工操作。
- 「禁用」语义收紧为自动维护终态：此前带 RT 的禁用号会被 watcher 刷新自动复活。
- 账号对象新增 `last_relogin_*` 与 `disabled_*` 诊断字段。

---

**完整变更**：见 [CHANGELOG.md](CHANGELOG.md)
**状态机说明**：见 [docs/account-lifecycle.md](docs/account-lifecycle.md)
**架构决策**：见 [docs/adr/0014-abnormal-account-recovery-and-disable.md](docs/adr/0014-abnormal-account-recovery-and-disable.md)

---

<!--
发布说明草稿。GitHub Actions 在当前 fork 上被暂停，Release 需手动创建：
  1) 打开 https://github.com/IFuZYI/chatgpt2api/releases/new
  2) 选择已存在的 tag v2.2.3
  3) 标题填 "ChatGPT2API 2.2.3"，正文粘贴本文件（去掉本注释块）
  4) 上传 /home/chatgpt2api-release-v2.2.3/ 下的 7 个文件
     （6 个平台二进制 + SHA256SUMS.txt）
-->
