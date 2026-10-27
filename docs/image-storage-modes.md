# 图片存储三模式：`local` / `webdav` / `both`

`image_storage.mode` 决定图片往哪儿写。三种模式**只在「写入」这一步有差异**——
读取、删除、URL 返回走的是同一段代码。

本文逐环节拆开，并给出实测数据。

---

## 一、配置与前置条件

```json
{
  "image_storage": {
    "enabled": true,
    "mode": "both",
    "webdav_url": "https://webdav.example.com/dav",
    "webdav_username": "your-user",
    "webdav_password": "your-password",
    "webdav_root_path": "chatgpt2api/images",
    "public_base_url": ""
  }
}
```

### `enabled` 是总开关，会覆盖 `mode`

归一化逻辑（`internal/config/normalize.go:53`）：

```go
enabled := boolOr(source["enabled"], false)
if !enabled {
    mode = "local"      // ← 无论你写 webdav 还是 both，都会被强制打回 local
}
```

**这是最容易踩的坑**：只改 `mode` 却忘了开 `enabled`，表面配置看着对，
实际一直是 `local`。设置页保存时会做同样的归一化，所以从界面看也会变回 `local`。

### `mode` 的非法值一律降级为 `local`

```go
switch mode {
case "local", "webdav", "both":
default:
    mode = "local"
}
```

写错大小写（`WebDAV`）或拼写错误（`dav`）都会静默变成 `local`。
注意：**没有 `dav` 这个取值**，正确的名字是 `webdav`。

### `webdav` / `both` 必须有密码

`mode` 设为 `webdav` 或 `both` 时 `webdav_password` 不能为空，
否则保存设置返回 `400 启用 WebDAV 图片存储后必须填写 WebDAV 密码`。
这是保存时的前置校验，不是运行时错误。

---

## 二、写入：三种模式唯一的差异点

入口：`internal/service/image.go` 的 `SaveImageBytes`。

### 文件名规则（三种模式相同）

```
YYYY/MM/DD/<unix秒>_<md5全32位>.png
例：2026/10/24/1761308548_bf7c0626cb9ff66e3b516044037a2f1c.png
```

`md5` 是图片内容的完整摘要（32 个十六进制字符）。文件名带 `unix秒`，
所以**同一张图在不同秒保存会得到不同文件名**；同一秒内重复保存才会撞名
（此时后写覆盖先写）。

### `local`

```
1. MkdirAll(目录)
2. 写本地文件
3. 索引：storage=local, local=true, webdav=false, remote_url=""
4. 返回
```

- **耗时**：约 1ms（纯磁盘操作）
- **失败**：写盘失败即整次失败（磁盘满、权限不足）
- **磁盘占用**：全部图片

### `webdav`

```
1. 建远端目录（MKCOL，逐级）
2. PUT 图片（同步，等响应）
3. 索引：storage=webdav, local=false, webdav=true, remote_url=<远端 URL>
4. 返回
```

- **耗时**：实测约 **300ms**（每请求 50ms 的模拟服务上：5 次 MKCOL + 1 次 PUT = 6 个请求）
  真实 WebDAV 实测 **11–20 秒**
- **失败**：**整次保存失败**，返回错误给调用方
- **磁盘占用**：0（本地不落盘）

> 为什么 `webdav` 失败要整次失败？因为本地没有副本。
> 若静默返回成功，用户会以为图已存好，实际什么都没留下。
> 见 [ADR-0011](./adr/0011-both-mode-degrades-on-webdav-failure.md)。

### `both`

```
1. 写本地文件
2. 索引：storage=local, local=true, webdav=false     ← 先如实标 local
3. 立即返回（不等远端）
   └─ 后台 goroutine 启动：
      a. MKCOL 逐级建目录
      b. PUT 图片
      c. 成功 → 索引改为 storage=both, webdav=true, remote_url=...
         失败 → 索引保持 local，往 stderr 打一行日志
```

- **耗时**：**约 2ms**（与 `local` 基本相同，远端在后台）
- **失败**：远端失败**不影响生图响应**，本地副本仍可读
- **磁盘占用**：全部图片（本地始终有副本）

> 为什么异步？同步实测要 11–20 秒，会把「生图耗时」变成「生图 + 远端」。
> 见 [ADR-0012](./adr/0012-both-mode-async-upload.md)。

### 实测对比

用带 50ms 延迟的假 WebDAV 实测（数据来自本仓库的临时基准测试）：

| 模式 | `SaveImageBytes` 返回耗时 | MKCOL 次数 | PUT 次数 |
|:--|--:|--:|--:|
| `local` | **1ms** | 0 | 0 |
| `webdav` | **307ms** | 5 | 1 |
| `both` | **2ms** | 5（后台） | 1（后台） |

MKCOL 是 5 次而不是 3 次，因为路径是
`chatgpt2api/images`（`webdav_root_path` 两层）+ `2026/10/24`（三层）= 5 层目录，
每层一次 MKCOL。

> 这张表的请求次数由 `TestStorageModeRequestCountsMatchDocumentedTable`
> 钉住（改实现导致次数变化会直接失败），耗时列只作量级参考。

> **优化提示**：`webdav_root_path` 每多一层目录，就多一次 MKCOL。
> 如果你的 WebDAV 服务端延迟高，把 root path 设浅一点（比如 `images`）能省请求。
> 同一天的第二张图仍会重复 MKCOL —— 代码没有「目录已存在」的缓存，
> 只靠 405 响应判断，但那已经花掉了一次往返。

### WebDAV 失败时的行为差异（实测）

WebDAV 返回 401 时：

| 模式 | 请求结果 | 本地文件 | 索引 `webdav` | PUT 尝试 |
|:--|:--|:--|:--|--:|
| `local` | ✅ 成功 | 有 | false | 0 |
| `webdav` | ❌ **失败** | 无 | false | 1 |
| `both` | ✅ 成功 | **有** | false | 1 |

`both` 的索引如实停在 `storage=local`，`remote_url` 为空。
事后可以在设置页跑「同步」补传（`POST /api/image-storage/sync`）。

---

## 三、读取：三种模式同一条路径

入口：`ReadImage`（`internal/service/image.go:465`）。

```
1. 读本地文件
   └─ 命中 → 返回（三种模式都会先走这步）
2. 未命中 → safeRelativePath 校验 + isImageRel + indexedOnWebDAV(rel)
3. 索引 webdav == true → GET 远端 → 返回
4. 否则 → 404 image not found
```

各模式的实际表现：

| 模式 | 本地命中 | 远端回源 | 说明 |
|:--|:--|:--|:--|
| `local` | ✅ | ❌ 永不 | 本地没图就 404 |
| `webdav` | ❌ 永不命中 | ✅ 总是 | 全靠远端 |
| `both` | ✅ 优先 | 本地缺失时 | 本地为主，远端兜底 |

关键点：**回源的条件是索引里 `webdav == true`**，不是「文件不存在就盲试远端」。
所以 `both` 模式后台上传失败（索引停在 `local`）时，本地文件若被删掉，
这张图就彻底 404 了 —— 索引不会去远端碰运气。

### 缩略图

`EnsureThumbnail` 也走 `ReadImage` 取源图，因此：

- `local`：本地出图
- `webdav`：每张缩略图都要回源一次（首次生成后缓存在 `data/image_thumbnails/`）
- `both`：本地出图，快

`webdav` 模式下管理页首次加载会很慢 —— 每张图都要先下载原图才能生成缩略图。

---

## 四、删除：与模式无关

入口：`DeleteImages`（`internal/service/image.go:539`）。

```
对每个 rel：
1. 本地文件存在 → os.Remove
2. indexedOnWebDAV(rel) == true → DELETE 远端（404 视为成功）
3. 删缩略图
4. 清标签
5. 从索引移除
```

三种模式跑的是同一段代码，不需要分支：

- `local`：只命中第 1 步
- `webdav`：只命中第 2 步（本地无文件）
- `both`：两步都命中

> **中间态隐患**：`both` 模式下，写入返回后到后台上传成功前（11–20 秒窗口），
> 索引是 `webdav=false`。若在这段时间删图，第 2 步会被跳过 ——
> 远端不会留下副本（因为它还没传上去），但**后台上传仍会继续**，
> 结果是远端多了一个孤儿文件。概率低，但存在。

---

## 五、返回的 URL：与模式无关

`publicImageURL`（`internal/service/image.go:203`）：

```
配了 public_base_url → <public_base_url>/<rel>
否则              → <baseURL>/images/<rel>
```

**从不返回 WebDAV 地址**，即使 `webdav` 模式也是如此。

这意味着 `webdav` 模式下客户端取图仍然经过本服务转发（服务回源再返回），
而不是直连对象存储。好处是权限统一、URL 稳定；代价是流量过本服务。

想让它直连远端，只能靠 `public_base_url` 指向一个能直接访问那批文件的地址
（比如把 WebDAV 目录挂到 nginx 上）。但那样 `/images/<rel>` 的路径规则
必须与你挂载的目录结构一致，否则 404。

---

## 六、进程退出时的行为

`both` 模式的异步上传由 `ImageService.Close(ctx)` 等待（`WaitGroup`）。

关闭顺序（`cmd/server/main.go`）：

```
1. server.Shutdown(10s)
2. 停 watcher / cleanup scheduler
3. deps.Tasks.Close(30s)          ← 等生图任务
4. deps.EditableTasks.Close(30s)  ← 等 PPT/PSD 任务
5. deps.Images.Close(taskCtx)     ← 等后台上传
```

**注意**：第 3–5 步**共用同一个 30 秒的 `taskCtx`**。

如果生图任务吃满了 30 秒，第 5 步的 `taskCtx` 已经过期，
`Images.Close` 会立即返回 `context deadline exceeded`，
于是**最后几张图只传了一半或完全没传**。

日志表现：

```
[server] pending image uploads: context deadline exceeded
```

看到这行就说明有图没传完。事后在设置页跑「同步」可以补传。

> 这也是 compose 里 `stop_grace_period: 45s` 的由来：
> Docker 默认只给 10 秒就 `SIGKILL`，比上面这条链还短，会直接截断落盘。

---

## 七、如何选

| 场景 | 推荐 | 理由 |
|:--|:--|:--|
| 单机、图片量小、不关心远端 | `local` | 最快，无外部依赖 |
| 想彻底不占本地盘、WebDAV 稳定 | `webdav` | 本地零占用 |
| 要可靠性、不想让生图等远端 | **`both`** | 远端故障不阻断生图 |
| 磁盘紧张 + WebDAV 偶尔不稳 | `both` + 设 `image_min_free_mb` | 本地满了会自动清旧图 |

`both` 是默认推荐：它是唯一「远端出问题也不影响用户拿到图」的模式。

---

## 八、排查

### 设了 `webdav` 但图片都落在本地

`enabled` 没开。检查：

```bash
curl -H "Authorization: Bearer <auth-key>" http://localhost:3000/api/settings \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['config']['image_storage'])"
```

看返回的 `enabled` 与 `mode`。若 `enabled` 是 `false`，`mode` 一定被强制成 `local`。

### `both` 模式下远端没有图

1. 看服务日志有没有 `[image] webdav put failed, kept local copy only: ...`
2. 到设置页点「测试」验证 WebDAV 连通性
3. 点「同步」手动补传（`POST /api/image-storage/sync`）

索引里查这张图是不是 `storage=local`、`webdav=false`：

```bash
python3 -c "
import json
d=json.load(open('data/image_index.json'))
for rel,item in d.items():
    if 'your-file' in rel: print(rel, item)
"
```

### `webdav` 模式生图特别慢

正常。每次保存都要 5 次 MKCOL + 1 次 PUT，真实 WebDAV 上 11–20 秒。
换 `both` 可以把这段移出响应路径。

### 关闭时日志出现 `pending image uploads`

后台上传没等到。见上文「进程退出时的行为」。
把 `stop_grace_period` 提到 45s+，或缩短生图超时让任务更早收尾。

---

## 相关文档

- [ADR-0011](./adr/0011-both-mode-degrades-on-webdav-failure.md)：`both` 为何降级而非失败
- [ADR-0012](./adr/0012-both-mode-async-upload.md)：`both` 为何异步
- [配置说明](./configuration.md)：`config.json` / `.env` / compose 的关系
- [部署文档](./deployment.md)：图片存储的部署与备份
