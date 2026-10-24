# 科学变更记录（SCIENTIFIC_CHANGELOG）

本文件记录本轮审计**改变行为语义**的改动，以及每项的证据状态。

改动分三类：
- **等价修复**（EQUIVALENT）：修正 Go 与 Python 的偏差，恢复原有语义
- **缺陷修复**（DEFECT）：Go 侧的独立 bug，Python 无对应问题
- **行为变更**（BEHAVIOR）：Go 与 Python 都不这样，本轮新决定

---

## 一、等价修复（恢复 Python 语义）

### 1. 历史前缀分隔符（commit 46612b5）
- **改动**：`joinHistoryText` 从 `strings.Join(filtered, "\n\n")` 改为 `""`
- **依据**：`services/protocol/conversation.py:210` 是 `"".join(...)`
- **影响**：多轮对话时历史剥离恢复生效
- **证据状态**：已验证（单元测试 `TestJoinHistoryTextUsesEmptySeparator`）

### 2. 工具提示分隔符（commit 46612b5）
- **改动**：`BuildToolPrompt` 在工具块之间插入 `\n`
- **依据**：`services/protocol/anthropic_v1_messages.py:49` 是 `"\n".join(blocks)`
- **影响**：多工具场景下上游能正确解析
- **证据状态**：已验证（`TestBuildToolPromptSeparatesToolsWithNewline`）

### 3. chunk 的 created 默认值（commit 46612b5）
- **改动**：`ImageOutput.ToChunk()` 在 `Created == 0` 时填当前 unix 时间
- **依据**：`conversation.py:330` 是 `field(default_factory=lambda: int(time.time()))`
- **影响**：前端按时间排序/去重恢复
- **证据状态**：已验证（`TestToChunkFillsCreatedWhenZero`）
- **残留差异**：跨秒两次调用会得到不同值（值接收者，不回填）。生产每 output 只调一次，无影响

### 4. n 越界校验（commit 47e26dc）
- **改动**：`generations` 越界返回 422（Pydantic 条目），`edits` 返回 400
- **依据**：`api/ai.py:27` `n: int = Field(default=1, ge=1, le=4)`；`api/image_inputs.py:50-60` 用 `HTTPException(400)`
- **影响**：用户能看出参数错了，而不是收到 502
- **证据状态**：已验证（`TestImageGenerationsRejectsOutOfRangeN` / `TestImageEditsRejectsOutOfRangeN` + 实测矩阵）

### 5. 异步任务 response_format（commit 94cedf4）
- **改动**：`b64_json` → `url`
- **依据**：`services/image_task_service.py:139,165` 用 `"url"`
- **影响**：`image_tasks.json` 从 31MB 降到只存 URL；内存占用同步下降
- **证据状态**：已验证（`TestImageTaskUsesURLResponseFormat` + 前端 `page.tsx:191,208` 本就支持 url）

### 6. 图片换号重试预算（commit 46612b5 + a729b14）
- **改动**：新增四类预算（text-reply 3 / TLS 3 / conn-timeout 3 / poll-timeout 4）
- **依据**：`conversation.py:1301-1307`
- **尝试次数**：4/4/4/5（首次 + 预算次重试），与 Python 的 `count += 1; if count <= MAX` 一致
- **影响**：某账号慢/限流不再必然导致整张图失败
- **证据状态**：已验证（三个测试 + 尝试次数逐一核对）

### 6.5 周期性备份调度器（commit ba52a2b）
- **改动**：新增 `service.BackupScheduler`，每 30 秒检查 `backup.enabled` / `interval_minutes`
- **依据**：`services/backup_service.py:326-351` `_run()` + `run_scheduled_backup_if_needed`
- **影响**：自动备份真的会发生（此前配置只保存不执行）
- **证据状态**：已验证（5 个单元测试 + 真实服务实测：配 `enabled=true, interval=1` 后 8 秒内日志出现备份尝试）

### 6.6 敏感词与 AI 审核（commit 5935c63）
- **改动**：新增 `service.ContentFilter`，接入 5 个请求入口
- **依据**：`services/content_filter.py:150-264`
- **影响**：管理员配的敏感词真的会拦截
- **证据状态**：已验证（7 个单元测试 + 3 个端到端测试 + 实测：配 `sensitive_words` 后含该词的请求返回 400 且错误体与 Python 逐字一致）

---

## 二、缺陷修复（Go 侧独立 bug）

### 7. 未知 API 路径被 SPA 吞成 200 HTML（commit 46612b5）
- **改动**：`NoRoute` 增加 `isAPIPath` 判断
- **Python 行为**：FastAPI 对未知 API 返回 404 JSON
- **影响**：SDK 不再把 HTML 当成功响应解析
- **证据状态**：已验证（`TestUnknownAPIPathsReturnJSON404` + 实测）

### 8. 备份不含账号与密钥（commit 46612b5）
- **改动**：`runBackup` 注入 `Accounts` / `Auth`
- **影响**：备份恢复后账号与密钥不再丢失
- **证据状态**：已验证（`TestBackupBuilderIncludesSnapshots`）

### 9. watcher panic 杀死 goroutine（commit 46612b5）
- **改动**：`tick` 加 `defer recover()`
- **Python 行为**：`api/support.py:87-109` 整轮 try/except
- **影响**：限流账号不会因一次 panic 永久不恢复
- **证据状态**：已验证（`TestWatcherTickRecoversFromPanic`）

### 10. 对话清理用空 token（commit 46612b5）
- **改动**：`removeConversationLater` 接收并透传生成时的 token
- **影响**：`image_remove_conversation_*` 开了真的能删
- **证据状态**：主链路已验证；**resume-poll 分支仍无 token**（见下「未解决」）

### 11. Docker 镜像无法构建/启动（commit 1ecb8c3）
- **改动**：
  - `.dockerignore` 不再排除 `CHANGELOG.md`（构建期被 `next.config.ts:21` 读取）
  - 容器内端口 `:80` → `:8080`（distroless nonroot 无法绑 <1024）
  - 新增 `--healthcheck` 子命令（distroless 无 curl/wget）
- **证据状态**：已验证（`docker build` 成功 65.5MB、容器启动、端点 200、healthcheck 退出 0）

---

## 三、行为变更（两边都不这样）

### 12. 图片并行路径也走重试（commit a729b14）
- **改动**：`runParallel` 从直接 `generator.Generate` 改为 `generateWithRetry`
- **理由**：`image_parallel_generation` 默认 true，不接就等于默认配置零重试
- **偏离 Python 吗**：不偏离 —— Python 的并行线程里本来就带重试循环（`conversation.py:1301+`）
- **证据状态**：已验证

### 13. TLS 分类读原始错误（commit a729b14）
- **改动**：`ImageGenerationError` 加 `Cause` 字段 + `Unwrap()`，分类器优先读它
- **理由**：`wrapImageError` 把错误改写成固定文案，`IsTLSConnectionError` 只认原文
- **证据状态**：已验证（包装后的错误仍分类为 `retryTLS`）

---

## 未解决（诚实记录）

| 项 | 状态 | 影响 | 为何未修 |
|:--|:--|:--|:--|
| resume-poll 无 token | 缺口 | 「继续等待」必 401 | 任务记录不存 `access_token`，补需改 `data/image_tasks.json` 字段集，属独立决策 |
| codex 响应格式 | 未验证 | 若上游用分片事件会报错 | 需真实 codex 账号抓包，当前号池全是 free web 号 |
| ~~周期性备份调度器~~ | ✅ 已修（ba52a2b） | — | — |
| ~~敏感词 / AI 审核~~ | ✅ 已修（5935c63） | — | — |
| 流式真增量 | 未实现 | 长请求期间客户端收不到字节 | 需重构 SSE 写出路径，属独立任务 |
| storage 包覆盖率 4.9% | 低 | 四个后端几乎无测试 | 需补大量测试，属独立任务 |
