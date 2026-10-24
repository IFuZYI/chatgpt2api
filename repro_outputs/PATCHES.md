# 变更清单（PATCHES）

本轮审计期间的提交：

47e26dc fix(image): n 越界返回校验错误，不再静默夹取后打到上游
94cedf4 perf(image): 异步任务改用 url 格式，image_tasks.json 不再存 base64
a729b14 fix(image): C8 重试预算接上生产路径（并行/分类/文本回复）
1ecb8c3 fix(docker): 镜像可绑定端口、可构建、有健康检查；文档纠正不实承诺
46612b5 fix: 修复全量检测发现的 8 个 Critical
9e05dd5 style: gofmt model_catalog.go
864ab33 perf(models): 缓存命中不再遍历号池，/v1/models 从 63.7ms 降到 5.8ms
a4c0b43 feat(image): image_storage.mode 生效（local/webdav/both）+ 落盘失败必须冒泡（D3-I17）
e16926a fix(shutdown): 关闭时取消并等待所有写 data/ 的后台工作，JSON 后端改原子写
c9615ba fix(codex): 跨事件去重真正生效，codex 流加 1200s 上限
88cb5df docs(adr): 0010 codex 响应格式是推断的，请求格式是逐字对齐的
0a89f0a fix(service): 图片任务服务加关闭语义，等待在途 goroutine 写完 image_tasks.json
1015c8d feat(upstream): codex 图片模型走 /backend-api/codex/responses（D3-C4）
478229f fix(protocol): 像素上限改用 Pillow 硬错误阈值 178_956_970
114a055 fix(protocol): 限制解码尺寸、跳过 1:1 缩放、mask 失败进 call 日志
9a1ef2b fix(protocol): WebP mask 按容器 alpha 标志位判定模式
44c1c55 refactor(protocol): 去掉 mask 模式里的冗余分支
560b87a feat(protocol): 局部重绘合成 mask（D3-I6）

## 文件级改动统计

 internal/protocol/image_retry_test.go              | 409 +++++++++++
 internal/protocol/testdata/mask_golden.json        | 752 +++++++++++++++++++++
 internal/service/account.go                        |  78 +++
 internal/service/account_version_test.go           |  69 ++
 internal/service/background.go                     |  92 +++
 internal/service/backup_snapshot_test.go           |  52 ++
 internal/service/image.go                          | 171 ++++-
 internal/service/image_helpers.go                  |  27 +
 internal/service/image_ops.go                      |  33 +-
 internal/service/image_storage_mode_test.go        | 602 +++++++++++++++++
 internal/service/image_task.go                     | 147 +++-
 internal/service/image_task_close_test.go          | 249 +++++++
 internal/service/model_catalog.go                  |  51 +-
 internal/service/model_catalog_bench_test.go       |  68 ++
 internal/service/model_catalog_cache_test.go       |  81 +++
 internal/service/watcher.go                        |  12 +
 internal/service/watcher_panic_test.go             |  56 ++
 internal/service/webdav.go                         |  23 +-
 internal/storage/json.go                           |  42 +-
 internal/storage/json_test.go                      | 103 +++
 internal/upstream/client.go                        |   1 -
 internal/upstream/codex_image.go                   | 186 +++++
 internal/upstream/codex_image_test.go              | 395 +++++++++++
 internal/upstream/image_chain.go                   |  16 +-
 70 files changed, 7680 insertions(+), 229 deletions(-)
