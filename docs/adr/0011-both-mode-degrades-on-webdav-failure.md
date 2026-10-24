# ADR-0011：`both` 模式在 WebDAV 失败时降级为本地，而不是让请求失败

## 背景

`image_storage.mode` 有三个值：`local`（只存本地）、`webdav`（只存远端）、
`both`（双写）。

原实现（照抄 Python `image_storage_service.py:205-243`）的顺序是：

```
if mode in {local, both}: 写本地
if mode in {webdav, both}: PUT 远端；失败则抛异常
```

于是 `both` 模式下远端失败时，**本地文件已经写好了，但整个请求返回 502**。
本地文件成了孤儿：存在，但没进索引，用户拿不到 URL。

实测：`image_storage.mode=both` + WebDAV 返回 401 时，
`POST /v1/images/generations` 返回
`502 {"error":{"message":"WebDAV MKCOL failed: HTTP 401"}}`，
而图片本身已经生成成功。

## 决定

**`both` 模式下远端失败不使请求失败。**

- 返回 200，URL 指向本地副本
- 索引如实标 `storage=local` / `local=true` / `webdav=false` / `remote_url=""`
- stderr 记一条 `[image] webdav put failed, kept local copy only: ...`
- 可事后用 `SyncToWebDAV` 补传

**`webdav` 模式行为不变**：本地没有副本，远端失败必须报错。
静默降级成「只存本地」会让用户以为图在远端 —— 那是更严重的 bug
（见 `TestWebDAVOnlyModeStillFailsOnRemoteError`）。

## 理由

本地已有副本时，远端抖动不该让用户拿不到图。`both` 的语义是
「两端都存」，不是「两端都成功才算成功」。

同时**不假装成功**：索引如实反映 `webdav=false`，调用方看得出这张图
没上远端。这与「静默降级」的区别在于 —— 静默降级是标 `storage=both`
却只存了本地，这里标的是 `storage=local`。

## 与 Python 的偏离

Python 会在同样场景抛异常（`save()` 里 `WebDAVClient.put()` 直接抛）。
这是**有意的改进**，不是移植错误：

- Python 的实现没有考虑「本地已存好」这个中间状态
- 它也没有补传机制（Go 有 `SyncToWebDAV`）

## 代价

- 与 Python 行为不一致：同样的配置和故障，Python 报错、Go 成功
- 用户若不看索引，可能以为图已在远端

## 证据

- `internal/service/image_both_degrade_test.go`：3 个测试锁定三种模式的边界
  - `TestSaveImageBytesBothModeDegradesWhenWebDAVFails`：both 降级 + 索引如实标记
  - `TestDegradedBothModeImageIsReadable`：降级后图片可读
  - `TestWebDAVOnlyModeStillFailsOnRemoteError`：webdav 模式仍报错
- `internal/service/image_storage_mode_test.go`：
  `TestSaveImageBytesWebDAVFailureFailsSave` 已收窄为只覆盖 `webdav` 模式
- 实测：WebDAV 指向 `https://127.0.0.1:1/dav` 时，
  `POST /v1/images/generations` 返回 200，索引 `storage=local`，
  日志记录 `connect: connection refused`
