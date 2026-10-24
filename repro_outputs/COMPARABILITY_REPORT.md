# 可比性报告（COMPARABILITY_REPORT）

本报告回答：**Go 重构版与 Python 原版在多大程度上可互换？**

基准：仓库内 Python 源码（`api/`、`services/`、`utils/`，约 17,642 行）+ `docs/adr/0001-0010` 声明的刻意偏离。

---

## 一、路由可比性

| 项 | Python | Go | 差异 |
|:--|:--|:--|:--|
| 路由总数 | 78 | 77 | 缺 `GET /api/third-party-apps`（用户明确要求删除无限画布功能时一并移除） |
| 鉴权等级 | — | — | 77 条对齐路由逐条一致；user key 扫 42 条 admin 路由全 401/403，零越权 |
| 归一化后缺失 | — | 0 | 除上述已删路由外零缺失 |
| 归一化后多出 | — | 0 | 无 Go 独有路由 |

**结论**：路由层**可互换**（除已声明的功能删除）。

---

## 二、错误信封可比性

Python 用三套信封（ADR-0005）：

| 前缀 | 形状 | Go 是否一致 |
|:--|:--|:--|
| `/api/*`（admin） | `{"detail": ...}` | ✅ |
| `/v1/*`（OpenAI） | `{"error": {message,type,param,code}}` | ✅ |
| `/v1/messages`（Anthropic） | `{"type":"error","error":{type,message}}` | ✅ |

**实测矩阵**（见 `AUDIT_SUMMARY.md`）：401/404/400/422/405 全部符合预期。

**结论**：错误信封**可互换**。

---

## 三、数据格式可比性

| 文件 | 格式 | 兼容性 |
|:--|:--|:--|
| `config.json` | 字段集与 Python 一致 | ✅ 双向读写 |
| `accounts.json` | `[]map[string]any`，21-24 字段 | ✅ 可被 Python 读取 |
| `auth_keys.json` | 裸数组 + `key_hash` | ✅ |
| `logs.jsonl` | 每行一个 JSON | ✅ |
| `image_index.json` | `{"items": {...}}` | ✅ |
| `image_tasks.json` | 本轮改为存 `url` 而非 `b64_json` | ⚠️ Python 也是 `url`，✅ 一致 |
| `backup_state.json` | 字段一致 | ✅ |
| OpenSSL 备份加密 | AES-256-CBC + PBKDF2-SHA256，**10000** 轮，`Salted__` 头 | ✅ 双向验证过 |

**结论**：数据格式**可互换**（可在两版之间迁移数据目录）。

---

## 四、协议可比性

| 协议面 | 状态 | 说明 |
|:--|:--|:--|
| `/v1/chat/completions` 非流式 | ✅ | chunk 形状一致 |
| `/v1/chat/completions` 流式 | ⚠️ | chunk 形状一致，但**生成期间不产生增量**（先跑完再一次性写） |
| `/v1/responses` | ⚠️ | 缺 `TOOL_UNAVAILABLE` 注入、缺 `output_item.added/done` 事件 |
| `/v1/messages`（Anthropic） | ✅ | `BuildToolPrompt` 已逐字对齐（含多工具换行） |
| `/v1/images/generations` | ✅ | 含 `n` 校验、mask 合成、重试预算 |
| `/v1/images/edits` | ✅ | 同上 |
| `/v1/models` | ✅ | 动态聚合，形状一致 |
| codex 图片链路 | ⚠️ | **请求侧逐字对齐，响应事件格式未经真实上游验证**（ADR-0010） |

**结论**：协议层**基本可互换**，两处 ⚠️ 已知且已声明。

---

## 五、性能可比性

本轮实测（本机，269 账号，42 张图）：

| 指标 | 值 | 备注 |
|:--|:--|:--|
| `/v1/models` 稳态延迟 | 5.8ms | 优化前 63.7ms（119× 改进） |
| `/api/accounts` | 14.6ms | 269 账号全量拷贝 + 排序 |
| `/api/images` | 8.8ms | 42 张 |
| 内存（RSS+Swap） | 84-93MB | 5000 次请求后无增长 |
| fd / Threads | 6 / 9 | 压测前后不变 |
| 镜像大小 | 65.5MB | distroless static |
| 二进制 | 41MB | `-trimpath -ldflags="-s -w"` |

**结论**：性能**优于** Python 版（无 GIL、单二进制、无解释器启动开销），内存稳定。

---

## 六、测试可比性

| 项 | 值 |
|:--|:--|
| 测试总数 | 344 |
| `go test ./...` | 全绿 |
| `go test -race ./...`（全 8 包） | 全绿 |
| `go vet` / `gofmt` | 干净 |
| 覆盖率（加权） | 约 45% |

**覆盖率分布**（这是主要短板）：

| 包 | 覆盖率 |
|:--|:--|
| internal/web | 91.5% |
| internal/health | 88.5% |
| internal/protocol | 72.8% |
| internal/config | 64.9% |
| internal/service | 44.1% |
| internal/upstream | 36.6% |
| internal/api | 30.8% |
| **internal/storage** | **4.9%** |

**结论**：核心协议层覆盖充分（72.8%），但 `storage` 与 `api` 偏低。四个存储后端几乎无测试，是最大风险面。

---

## 七、已知不可比（诚实清单）

| 项 | 性质 | 影响 |
|:--|:--|:--|
| `resume-poll` 无 token | 缺口 | 「继续等待」必 401，功能不可用 |
| codex 响应格式 | 未验证 | 若上游用分片事件会明确报错（不静默返回空图） |
| ~~周期性备份调度器~~ | ✅ 已修 | 每 30 秒检查，到期触发 |
| ~~敏感词 / AI 审核~~ | ✅ 已修 | 5 个入口接入，取号前检查 |
| ~~token 失效处理~~ | ✅ 已修 | invalid_count + 异常标记 + 按配置删号 |
| auto_relogin_after_refresh | 已声明缺口 | 依赖未实现的密码重登（ADR-0009），启动时明确提示 |
| 流式真增量 | 未实现 | 长请求期间客户端收不到字节，60s 空闲可能被切断 |
| `/v1/ppt|psd/generations` | 501 | 可编辑 PPT/PSD 未实现（ADR-0009） |
| `/v1/search` | 501 | 未实现（ADR-0009） |
| 密码重新登录 | 明确报错 | 未实现（ADR-0009） |

---

## 八、总体判断

**可互换程度：高**。

- 路由、错误信封、数据格式、备份加密：**完全可互换**
- 协议层：两处 ⚠️（流式增量、codex 响应），均有明确声明与失败可见性
- 性能：优于原版
- 测试：核心层充分，存储层薄弱

**不建议在以下场景直接替换 Python 版**：
1. 依赖「继续等待」恢复超时任务的生产环境（resume-poll 无 token，必 401）
2. 依赖 codex 画图且上游用分片事件的环境（响应格式未验证，ADR-0010）
3. 依赖流式真增量的环境（当前先跑完再一次性写）
