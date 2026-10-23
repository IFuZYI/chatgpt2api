# 2.0 重写完成的范围，与刻意留下的空白

## 已完整移植（行为对齐 Python 版）

| 子系统 | 覆盖 |
|:--|:--|
| HTTP 层 | 全部 78 条路由，三套错误信封，CORS，SPA fallback，`_next/` 404 特判 |
| 配置 | `config.json` 全部归一化逻辑、`_existing_cf_*` cookie 保留机制、敏感字段脱敏 |
| 存储 | json / sqlite / postgres / git 四种后端，增量写入语义 |
| 认证 | 裸 sha256 + `hmac.Equal`，密钥 CRUD，`last_used_at` 60s 节流 |
| 号池 | 槽位（channel 信号量）、token 别名链、限流/异常状态、刷新进度、导出 |
| 上游 | TLS 指纹 + header 顺序、sentinel prepare/finalize、PoW（SHA3-512）、turnstile 解释器 |
| 协议 | SSE 状态机、增量拼接、注解清洗、`file_id` 提取、moderation 检测、chat/responses/messages/search |
| 图片 | 生成/编辑、并行池、轮询（settle/check-before-hit/退避）、对话清理、任务队列、resume-poll |
| 管理面 | 账号 CRUD、图片管理（列表/删除/压缩/清理到目标/缩略图/标签）、日志、代理测试与清障、备份（R2 SigV4）、WebDAV 同步、CPA、sub2api、OAuth PKCE 桥 |

## 刻意留下的空白（都返回明确错误，不假装成功）

| 项 | 当前行为 | 原因 |
|:--|:--|:--|
| 密码重新登录 | `re-login` 进度标 error，文案「未实现」 | 依赖完整的 sentinel→authorize→验证码→token 交换链路，与 OAuth 桥不同路径 |
| 可编辑 PPT/PSD | `/v1/ppt|psd/generations` 返回 501 | 需要解析上游 sandbox 产物（zip 里的 psd/pptx），链路长且未验证 |
| 网页搜索 | `/v1/search`、chat 的搜索分支返回 501 | 搜索链路需要轮询会话文档 + 来源提取，尚未移植 |
| Responses 图片工具 | `/v1/responses` 带 `image_generation` 工具时返回 501 | 图片链路已就绪，但 Responses 形态的图片输出组装未接 |
| 远程图片 URL 拉取 | `/v1/images/edits` 的 `http(s)://` 引用返回 400 | 需要出站客户端；data URL 与 multipart 已支持 |
| 匿名对话 | 依赖上游；当前上游限制（ADR-0008） | 非实现问题 |

**为什么留下这些**：它们的共同点是「链路长 + 无法在当前环境端到端验证」。把未验证的代码写成看起来能用的样子，比明确的 501 更危险——用户会以为功能可用，出问题时也无从判断是上游还是实现。每条都在 CHANGELOG 的「已知问题」里列出。

## 与 Python 版的实测差异（刻意保留）

1. **`download_images_zip` 保留 WebDAV 回退**：Python 版被同名函数覆盖导致该功能不可用，Go 版按修复后语义实现。
2. **空 token 不发 `Authorization` 头**：Python 会拼出 `Bearer `（尾部空格），上游判为无效凭据。
3. **JSON 对象键顺序不同**：Python 保留 dict 插入序，Go 按 key 排序。语义等价，golden 用语义比较而非字节比较（ADR-0003 修订）。
4. **golden 里 405 的 `Allow` 头**：Gin 与 FastAPI 生成的值不同，未逐字复刻（客户端不依赖）。

## 测试面

170 个 Go 测试：
- `internal/config` 11（归一化、默认值、cookie 保留）
- `internal/protocol` 117（SSE、缓存、token 计费、Anthropic、图片轮询/池）
- `internal/service` 35（备份加密双向 OpenSSL 兼容、R2 SigV4、WebDAV、OAuth PKCE）
- `internal/upstream` 5（PoW 跨语言 golden、live 握手）
- `internal/api` 2 组（28 个 golden 用例 + 78 条路由对齐）
- `internal/web` 4（embed、SPA fallback、路径穿越防护）

live 测试（`-tags live`）单独跑，只记录上游状态不设失败断言（ADR-0008 的教训）。
