# ADR-0012：`both` 模式的远端上传改为异步

## 背景

ADR-0011 让 `both` 模式在 WebDAV 失败时降级为本地（不再 502），
但上传仍然是**同步**的：`SaveImageBytes` 要等 PUT 完成才返回。

实测 WebDAV 的 PROPFIND/PUT 要 11-20 秒，直接叠加到生图耗时上。
用户诉求是「本地远端都存，但生图不等远端」。

## 决定

**`both` 模式的远端上传改为异步。**

顺序：
1. 写本地（同步，必须成功）
2. 索引标 `storage=local` / `webdav=false`（如实反映当前状态）
3. 立刻返回 200 + 本地 URL
4. 后台 goroutine 上传；成功后把索引改成 `storage=both` / `webdav=true` / `remote_url=...`
5. 上传失败只记日志，索引保持 `local`

**`webdav` 模式不变**：同步上传，失败报错（本地无副本，必须知道结果）。

## 读取语义（用户明确要求）

`ReadImage` 已经是「本地优先 → 本地缺失且索引 webdav=true 才回远端」：

```go
if payload, err := os.ReadFile(localPath); err == nil {
    return payload          // 本地命中，实测 12-33ms
}
if s.indexedOnWebDAV(rel) { return s.fetchRemoteImage(rel) }
return 404
```

所以异步上传期间（索引还是 `local`）读取完全正常，用户无感。

## 关闭语义

`ImageService.Close(ctx)` 等 `uploadWG`：进程退出前不留下半截上传。
已接入 `main.go` 的关闭序列（在 image tasks / editable tasks 之后）。

## 代价

- 索引存在短暂的「实际已上传但标 local」窗口（上传完成前）
- 进程被 SIGKILL 时，最后几张图可能只存了本地（索引如实标 local，可 `SyncToWebDAV` 补传）

## 证据

- `internal/service/image_async_webdav_test.go`：
  - `TestBothModeUploadsAsynchronously`：远端慢 500ms 时 SaveImageBytes < 300ms
  - `TestBothModeAsyncUploadUpdatesIndex`：索引最终变成 both
  - `TestBothModeAsyncUploadFailureKeepsLocal`：失败保持 local 且本地可读
  - `TestWebDAVOnlyModeStillSynchronous`：webdav 模式仍等待
- 实测：WebDAV 指向 `127.0.0.1:1`（必然失败）时，
  `POST /v1/images/generations` 返回 200，耗时 26.6s（全部是上游生图）
