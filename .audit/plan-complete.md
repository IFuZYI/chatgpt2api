# 实施计划：补全 Go 功能复现 + 性能优化 + 可靠性加固

## 目标
审计已发现 112 条问题，其中 27 条已修。本计划补齐剩余功能缺口，并针对性能与可靠性做加固。

## 权威依据（Spec）
- Python 源码 `services/` `api/` `utils/` —— 行为基准
- 审计报告 `.superpowers/sdd/plan/D1-report.md` … `D7-report.md` —— 缺陷清单
- 已修项见 `.superpowers/sdd/plan/progress.md`

## 全局约束
1. **行为对齐 Python**：除已记录偏离外，输出/状态码/字段名逐字一致
2. **不破坏 data/ 格式**：accounts.json 等文件结构不变
3. **每个任务必须带测试**：新增行为要有覆盖，回归要有断言
4. **不留死代码**：新增配置项必须被请求链路读取，新增接口必须有调用点
5. **测试必须真跑**：`go test ./...` 全绿 + `go test -race` 三个核心包全绿
6. **不引入 CGO**：`CGO_ENABLED=0` 必须能编译

## 任务分解（8 个任务，按依赖排序）

### Task 1: mask 局部重绘（D3-I6）

## 背景
用户在 `/v1/images/edits` 上传 mask 做局部重绘（OpenAI 标准用法）。Go 侧把 mask 直接丢弃，导致整张图被重画 —— 结果"看起来对但语义错"，比报错更难发现。

## 权威实现（Python）
`services/protocol/openai_v1_image_edit.py:20-49` 的 `_composite_mask`：

```python
def _composite_mask(images, masks):
    """将 mask 的 alpha 通道合成到图片中，标识需要编辑的区域。
    mask 的透明区域（低 alpha）= 需要编辑的区域，
    mask 的不透明区域（高 alpha）= 保留的区域。
    如果无 mask 则返回原图。
    """
    if not masks:
        return images
    result = []
    for i, (data, filename, mime_type) in enumerate(images):
        # mask 索引：有第 i 个用第 i 个，否则回退到最后一个
        mask_data = masks[i][0] if i < len(masks) else masks[-1][0]
        img = Image.open(BytesIO(data)).convert("RGBA")
        mask_img = Image.open(BytesIO(mask_data))
        # 取 alpha 通道：RGBA 取第 4 通道，L 直接用，其他转 L
        if mask_img.mode == "RGBA":
            alpha = mask_img.split()[3]
        elif mask_img.mode == "L":
            alpha = mask_img
        else:
            alpha = mask_img.convert("L")
        # 关键：resize 到原图尺寸，用 LANCZOS
        alpha = alpha.resize(img.size, Image.LANCZOS)
        img.putalpha(alpha)
        buf = BytesIO()
        img.save(buf, format="PNG")
        result.append((buf.getvalue(), filename, "image/png"))
    return result
```

调用点：`openai_v1_image_edit.py:53` — `images = _composite_mask(images, masks)`，在构造 ConversationRequest 之前。

## Go 现状
- `internal/api/image_generation_routes.go:112` — `_ = mask`（同步链路丢弃）
- `internal/api/image_task_runner.go` — `taskCtx.Masks` 从未被读取（异步链路丢弃）
- 图片类型统一是 `[3]string{base64, filename, mime}`（见 `protocol.ImageRequest.Images`）

## 要求
1. 在 `internal/protocol/` 新增 mask 合成函数，签名参考：
   `CompositeMask(images, masks [][3]string) ([][3]string, error)`
2. 语义必须与 Python 完全一致：
   - 无 mask → 原样返回
   - mask 索引回退规则：`i < len(masks) ? masks[i] : masks[len-1]`
   - 取 alpha：RGBA 取 alpha 通道；灰度直接用；其他转灰度
   - resize 到原图尺寸（Go 用 `golang.org/x/image/draw` 的 `CatmullRom`，它是 LANCZOS 的最近等价）
   - 输出统一 PNG（mime 固定 `image/png`，filename 保留原图名）
3. 同步链路接入：`serveImageRequest` 调用前合成
4. 异步链路接入：`runImageTask` 从 `taskCtx.Masks` 取 mask 并合成
5. 测试覆盖：
   - 无 mask 原样返回
   - 1 图 1 mask 合成后 alpha 通道存在且尺寸与原图一致
   - mask 数量少于图片时回退到最后一个
   - 非 RGBA 的 mask（灰度）也能处理

## 依赖可用性
`golang.org/x/image` 已在 go.mod 中（用于缩略图），`draw.CatmullRom` 可直接用。

## 完成标准
- `go build ./...` 通过
- `go test ./internal/protocol/ ./internal/api/` 全绿
- 新增测试覆盖上述 4 个场景

### Task 2: codex 图片模型（D3-C4）
Python 走 `/backend-api/codex/responses` + `tools=[{"type":"image_generation"}]`，按 `source_type=codex` 选号。
Go 现状：`codex-gpt-image-2` 被塞进 `/f/conversation`，全仓库无 `/codex/responses`。
要求：实现 codex 链路；若不可行，从 `/v1/models` 移除该模型并改文档（二者择一，需在报告里说明选择理由）。

### Task 3: WebDAV 存储模式生效（D3-I17）

## 背景
`image_storage.mode` 有三个值：`local`（只存本地）、`webdav`（只存远端，省本地空间）、`both`（双写）。Go 侧完全忽略这个配置，永远只写本地、索引硬写 `webdav:false`。设成 `webdav` 的用户本地磁盘照样涨满。

## 权威实现（Python `services/image_storage_service.py:205-256`）

### save（`:205-243`）
```python
def save(self, image_data, base_url=None):
    config.cleanup_old_images()
    rel = self.make_relative_path(image_data)
    mode = self.mode()                       # local | webdav | both
    if mode not in {"local", "webdav", "both"}:
        mode = "local"
    stored_local = stored_webdav = False
    remote_url = ""

    if mode in {"local", "both"}:
        path = _local_image_path(rel)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(image_data)
        stored_local = True

    if mode in {"webdav", "both"}:
        remote_url = WebDAVClient(self.settings()).put(rel, image_data)
        stored_webdav = True

    dimensions = _image_dimensions(image_data)
    item = {
        "rel": rel, "path": rel, "name": Path(rel).name,
        "date": "-".join(rel.split("/")[:3]),
        "size": len(image_data), "created_at": _now_iso(),
        "storage": "both" if stored_local and stored_webdav else ("webdav" if stored_webdav else "local"),
        "local": stored_local, "webdav": stored_webdav,
        "remote_url": remote_url,
    }
    if dimensions: item["width"], item["height"] = dimensions
    # 写索引
    return StoredImage(rel=rel, url=..., storage=str(item["storage"]), size=len(image_data))
```

### get_bytes（`:246-256`）
```python
def get_bytes(self, rel):
    safe_rel = _safe_relative_path(rel)
    if not _is_image_rel(safe_rel): raise 404
    path = _local_image_path(safe_rel)
    if path.is_file():
        return path.read_bytes()              # 本地优先
    item = self._load_clean_index().get(safe_rel, {})
    if item.get("webdav"):
        return WebDAVClient(self.settings()).get(safe_rel)   # 回退远端
    raise 404
```

## Go 现状
- `internal/service/image.go:584-620` — `SaveImageBytes` 只 `os.WriteFile`，索引硬写 `"storage":"local","local":true,"webdav":false`
- 全仓库 `grep -c "ImageStorageSettings" internal/service/image.go` = 1，且那 1 处只用于生成 URL（`publicImageURL`），不参与保存
- `internal/service/webdav.go` 已有 `WebDAVClient`（`Put`/`Get`/`Delete`），可直接复用
- `SyncToWebDAV` 只能手动触发，是补传工具，不是保存路径

## 要求
1. `SaveImageBytes` 按 `ImageStorageSettings()["mode"]` 分流：
   - `local`：只写本地
   - `webdav`：只 put 远端（不写本地）
   - `both`：两边都写
   - 非法值 → 按 `local` 处理
2. 索引字段严格按上表：`storage` 三态、`local`、`webdav`、`remote_url`
3. `ReadImage`（对应 Python `get_bytes`）本地缺失且索引 `webdav:true` 时回退远端读取
4. WebDAV 失败时的行为：**必须让 save 失败**（不能静默只存本地 —— 那会让 `webdav` 模式的用户以为图存好了）。错误信息要包含 HTTP 状态
5. 测试：
   - `local` 模式：不调 WebDAV，索引 `storage=local`
   - `webdav` 模式：不写本地，索引 `storage=webdav`、`local=false`
   - `both` 模式：两边都有，索引 `storage=both`
   - `get_bytes` 本地缺失时回退 WebDAV
   - WebDAV 失败时 save 返回错误（用 fake 服务器模拟 500）

## 依赖
`internal/service/webdav.go` 的 `NewWebDAVClient(settings)` + `Put(ctx, rel, payload, publicBase)` / `Get(ctx, rel)` 可直接用。测试可参考 `webdav_test.go` 里的 `webdavFake`。

## 完成标准
- `go build ./...` 通过
- `go test ./...` 全绿
- 新增测试覆盖上述 5 个场景

### Task 4: 图片与文本换号重试（D3-I8, D2-I）
Python `conversation.py:1301-1520` 四类预算：轮询超时换号 4 次、文本回复 3 次、TLS 3 次、token 失效换号。
Go 现状：一次失败直接返回。
要求：在 ImagePool 的 Acquire 层实现换号重试；文本链路同样。

### Task 5: 输入校验与内容过滤（D6-I, D3-I12, D3-I15, D3-I16）

## 背景
三处缺失，都属于"管理员/客户端以为生效但实际没有"：
1. `n` 越界被静默 clamp 到 4（Python 是 422/400 报错）
2. 敏感词 + AI 审核在图片/文本入口零调用
3. JSON 编辑请求丢掉 `b64_json`/`base64` 内联图片与 `image_url` 字段

## 5.1 n 校验

### Python
- `/v1/images/generations`（`api/ai.py:27`）：pydantic `n: int = Field(default=1, ge=1, le=4)` → 越界 **422**，Pydantic 形状
- 编辑链路（`api/image_inputs.py:50-60`）：
```python
def _parse_count(value):
    try:
        count = int(value or 1)
    except (TypeError, ValueError):
        raise HTTPException(400, {"error": "n must be an integer"})
    if count < 1 or count > 4:
        raise HTTPException(400, {"error": "n must be between 1 and 4"})
    return count
```

### Go 现状
`internal/api/image_generation_routes.go:78,105` 用 `clampImageCount(intOf(...))`，`0/-3→1`、`99→4`、`"abc"→1`。

### 要求
- `/v1/images/generations`：`n` 非整数或越界 → **422**，Pydantic 条目形状 `{"type":"less_than_equal","loc":["body","n"],"msg":"Input should be less than or equal to 4","input":<原值>,"ctx":{"le":4}}`
- `/v1/images/edits` 与两个任务接口：→ **400** `{"detail":{"error":"n must be between 1 and 4"}}` 或 `{"detail":{"error":"n must be an integer"}}`
- 删除 `clampImageCount`（不再需要）

## 5.2 内容过滤

### Python `services/content_filter.py:150-175`
```python
def check_request(text):
    if not text.strip(): return
    # 1. 本地敏感词（原始文本直接子串匹配，无网络）
    for word in config.sensitive_words:
        if word in text:
            raise HTTPException(400, {"error": "检测到敏感词，拒绝本次任务"})
    review = config.ai_review
    if not review.get("enabled"): return
    base_url = str(review.get("base_url") or "").strip().rstrip("/")
    api_key  = str(review.get("api_key") or "").strip()
    model    = str(review.get("model") or "").strip()
    if not base_url or not api_key or not model:
        raise HTTPException(400, {"error": "ai review config is incomplete"})
    fail_open = _resolve_fail_open(review)
    # ... 调远程审核，按 fail_open 决定失败时放行还是拒绝
```

调用点：`api/ai.py:101,114`（图片生成/编辑）、`api/image_tasks.py:56,83`（任务）、以及文本对话入口。

### Go 现状
- `internal/config/config.go:294` `SensitiveWords()` 与 `:309` `AIReview()` **只有 settings 回显，无任何请求链路调用**
- `grep -rln "ContentFilter\|content_filter" internal/` 无结果

### 要求
1. 新增 `internal/service/content_filter.go`：
   - `CheckRequest(ctx, text string) *HTTPError`（或返回 `error` + 状态码，由 api 层转信封）
   - 敏感词子串匹配，命中 → 400 `{"error":"检测到敏感词，拒绝本次任务"}`
   - AI 审核启用时调 `{base_url}/chat/completions`（OpenAI 兼容），按 `fail_open` 决定网络失败时放行/拒绝
   - 配置不完整 → 400 `{"error":"ai review config is incomplete"}`
2. 接入点：
   - `/v1/images/generations`、`/v1/images/edits`（prompt）
   - `/api/image-tasks/generations`、`/api/image-tasks/edits`（prompt）
   - `/v1/chat/completions`、`/v1/messages`（最后一条 user 消息的文本）
3. 审核请求要剥离 base64 大块（Python `_sanitize_for_review`），避免把几 MB 图片发给审核服务

## 5.3 JSON 编辑请求的图片字段

### Python `api/image_inputs.py:107-158`
- 遍历三个键：`("images", "image", "image_url")`
- `_source_from_object` 支持：
  - `b64_json` 字段（内联 base64，可带 `filename`/`mime_type`）
  - `base64` 字段（同上）
  - `image_url` 为 `{"url": "..."}` 嵌套形态
  - `url` 裸字段

### Go 现状
`internal/api/image_generation_routes.go:373-380` 只遍历 `("image","images")`；对象分支只读 `image_url`/`url`，**不看 `b64_json`/`base64`**，没有 url 就静默 `continue`。

### 要求
- 遍历 `("images","image","image_url")`
- 对象分支支持 `b64_json`、`base64`、`image_url`（字符串或 `{"url":...}`）、`url`
- 全都没有 → 不要静默跳过，收集错误并在最后统一报 400

## 测试要求
- `n=0`/`n=5` → 422（generations）/ 400（edits）
- `n="abc"` → 400 `n must be an integer`
- 敏感词命中 → 400，且**不发起上游请求**（用 fake 断言）
- 敏感词未命中 → 正常通过
- AI 审核未启用 → 跳过，不发网络请求
- `{"images":[{"b64_json":"..."}]}` 能解析出 1 张图
- `{"image_url":"data:image/png;base64,..."}` 能解析出 1 张图

## 完成标准
- `go build ./...` 通过
- `go test ./...` 全绿
- `go test -race ./internal/service/ ./internal/api/` 全绿
- 上述测试场景全覆盖

### Task 6: 流式真增量（D3-I9, D2-I）
Python 边读 SSE 边 yield chunk；Go 先跑完再一次性吐。
要求：`stream=true` 时按进度实时写出，TTFB 应显著下降。

### Task 7: 存储与备份加固（D5-C3, D5-C4, D5-M）

## 背景
三处数据完整性问题：sqlite 缺索引、备份归档路径与 Python 不一致、config.json 权限被放宽。外加 storage 包零测试覆盖（0%），这是 SQLite 密钥清空那个 bug 能活到现在的原因。

## 7.1 sqlite 显式索引

### Python `services/storage/database_storage.py:18,27`
```python
access_token = Column(String(2048), unique=True, nullable=False, index=True)
key_id       = Column(String(255),  unique=True, nullable=False, index=True)
```
SQLAlchemy 会建：
```sql
CREATE INDEX ix_accounts_access_token ON accounts (access_token)
CREATE INDEX ix_auth_keys_key_id      ON auth_keys (key_id)
```

### Go 现状
`internal/storage/database.go:70-88` 只建表，没有 `CREATE INDEX`。实测只有 UNIQUE 自动索引 `sqlite_autoindex_*`。

### 要求
- DDL 里补两条 `CREATE INDEX IF NOT EXISTS`，名字必须是 `ix_accounts_access_token` / `ix_auth_keys_key_id`（与 SQLAlchemy 一致，便于从 Python 迁移的库无缝衔接）
- sqlite 与 postgres 两个分支都要加

## 7.2 备份归档路径与内容

### Python `services/backup_service.py:611-649`
```python
metadata = {"version": 2, "created_at": ..., "trigger": ..., "app_version": ..., "storage_backend": ...}
self._add_bytes_to_archive(archive, "backup-metadata.json", ...)
if include.get("config"):          self._add_file_to_archive(archive, CONFIG_FILE, "config.json")
if include.get("cpa"):             ... "cpa_config.json"
if include.get("sub2api"):         ... "sub2api_config.json"
if include.get("logs"):            self._add_file_to_archive(archive, LOGS_FILE, "data/logs.jsonl")
if include.get("image_tasks"):     ... "data/image_tasks.json"
if include.get("accounts_snapshot"):
    self._add_bytes_to_archive(archive, "snapshots/accounts.json", ...)
if include.get("auth_keys_snapshot"):
    self._add_bytes_to_archive(archive, "snapshots/auth_keys.json", ...)
if include.get("images"):
    self._add_file_to_archive(archive, TAGS_FILE, "data/image_tags.json")
    self._add_directory_to_archive(archive, config.images_dir, "data/images")
```

### Go 现状
- 路径已部分修正（`data/logs.jsonl`、`data/image_tasks.json`、`data/image_tags.json`、`data/image_index.json`）
- **缺** `data/images` 目录的二进制文件（`include.images` 只补了 index/tags）
- metadata 已含 `version:2` / `app_version` / `storage_backend`（上一轮修过）

### 要求
- 补 `data/images` 目录遍历，把图片二进制按 `data/images/<rel>` 路径写进归档
- 注意：图片可能几十 MB，要有大小上限保护（参考 Python 无上限，但 Go 侧建议设 500MB 上限并记日志）

## 7.3 config.json 权限

### Go 现状
`internal/storage/json.go:48` `os.WriteFile(path, payload, 0o644)` —— 把原本 0600 的 config.json 放宽到 0644。

### 要求
- config.json 写回保持 **0600**
- accounts.json / auth_keys.json 等数据文件保持 0644（与原行为一致）
- 区分方式：`config.Store` 的写路径用 0600，storage 后端的写路径用 0644

## 7.4 storage 包测试（当前 0%）

### 要求
至少覆盖：
- json 后端：Load/Save 往返、`{"items":[...]}` 与裸数组两种格式、坏元素容错（`loadJSONList` 逐条解析）
- sqlite 后端：建表、账号往返、**auth key 往返（这是数据丢失 bug 的回归点）**、增量同步（新增/更新/删除）
- 索引存在性断言（`sqlite_master` 里能查到 `ix_*`）

## 完成标准
- `go build ./...` 通过
- `go test ./...` 全绿
- `go test -cover ./internal/storage/` 覆盖率 > 0（目标 40%+）
- 新增测试覆盖上述场景

### Task 8: 性能优化与并发加固（D5-I, D3-I7, 控制器实测）

## 背景
四个可测的性能/并发问题。每个都要有前后对比数据。

## 8.1 模型目录 ensure() 竞态（D5-I）

### 现象
`internal/service/model_catalog.go:125` 写 `c.expiresAt`，`:104` 读它，但读写不在同一把锁下。`go test -race` 可复现 DATA RACE。并发下会打出多轮上游请求。

### 要求
- `ensure()` 改成 double-checked locking，或整体持锁
- `go test -race ./internal/service/` 必须无 DATA RACE
- 补测试：N 个 goroutine 同时调 `ListModels`，断言上游 fetch 只被调用 1 次

## 8.2 图片任务 JSON 膨胀（D3-I7）

### 现象
`internal/api/image_task_runner.go:60` 硬编码 `ResponseFormat: "b64_json"`，导致每张图的 base64（1.2-1.6MB）都写进 `data/image_tasks.json`。而 `image_task.go:156-178` 的 `saveLocked` **每次进度回调**都整文件重写。

实测：2 个任务 = 2.67MB 文件；10 个任务 = 数十 MB，每次进度更新都全量写盘。

Python `image_task_service.py:133-141` 用 `"response_format": "url"`，只存 URL。

### 要求
- 任务链路改用 `url` 格式（不存 b64）
- 前端 `/api/image-tasks` 读 `data[].url` 渲染，不依赖 b64（需核对 `web/src/app/image/page.tsx`）
- 测试：任务完成后 `data[0]` 有 `url`、无 `b64_json`

## 8.3 图片索引全量读盘

### 现象
`internal/service/image.go:113` `loadIndexLocked()` 每次 `ListItems` / `SaveImageBytes` 都从磁盘读整个 `image_index.json` 并解析。30 张图时 9.3KB 无感，1000 张时会明显。

### 要求
- 加内存缓存 + mtime 校验（文件变了才重读）
- 写路径更新缓存
- 测试：连续 100 次 `ListItems` 只读盘 1 次（用可注入的 reader 计数）

## 8.4 /api/accounts 全量拷贝

### 现象
`internal/service/account.go:360-380` 的 `ListAccounts` 每次调用都 `copyMap` 全部账号 + 排序。269 个账号时每次约 269 次 map 拷贝。

### 要求
- 先测量：`curl /api/accounts` 的耗时与分配
- 优化：缓存排序后的 token 顺序（账号增删时失效），减少重复排序
- 保留 `copyMap`（调用方可能修改返回值），但可以用池化或减少字段拷贝
- **必须给出优化前后的 benchmark 数据**

## 测试与验证要求
每个子项都要：
1. 优化前的测量数据（benchmark 或 curl 计时）
2. 优化后的测量数据
3. 回归测试保证行为不变

## 完成标准
- `go build ./...` 通过
- `go test ./...` 全绿
- `go test -race ./internal/service/ ./internal/api/` 全绿
- 报告里必须有 4 组前后对比数据

## 验证标准
每个任务完成后：
1. `go build ./...` 通过
2. `go test ./...` 全绿
3. `go test -race ./internal/{service,api,protocol}` 全绿
4. 该任务的新行为有测试覆盖
5. 真实端到端验证（服务能起来，相关接口能通）
