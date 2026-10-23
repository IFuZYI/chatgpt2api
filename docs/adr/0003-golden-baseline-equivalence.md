# 用 Python 录制的 golden 快照证明 Go 版行为等价

36 个 Python 测试里，10 个要连活服务、4 个用 `TestClient` + mock、5 个读已不存在的 `assets/*.png`、还有 24 个在当前仓库**本来就是红的**（见下方基线）。逐个翻译成 Go 表驱动测试工作量大，且翻译过程会顺手把 bug 一起"修"掉——那就不再是重构，是重写加改需求。

改为在 Python 版上录制响应快照，存成 `testdata/golden/*.json`，Go 版跑同一套用例逐字节比对。覆盖范围：`/v1/models`、`/v1/chat/completions` 文本、`/v1/messages`、`/v1/responses`、全部 `/api/*` 的 401/403/400/404/422/429/502 错误信封。三套信封必须完全一致：管理面 `{"detail":...}`、`/v1/*` `{"error":{...}}`、Anthropic `{"type":"error","error":{...}}`。

上游相关用例不录真实上游，用 `httptest` 假上游喂固定的 conversation SSE——`docs/upstream-sse-conversation.md` 已把 payload 形态写全，可直接当 fixture 来源。

## 录制基线（重构前的实测状态）

`uv run python -m pytest test/ -q` → **132 passed, 24 failed**。这 24 个红测试在重构前就红，不是 Go 化引入的：

| 原因 | 数量 | 说明 |
|:--|:--|:--|
| 断言与实现不符（测试过期） | 8 | `test_v1_images_edits_json.py` 断言 `image_1.png` / `"remote image URLs are not supported"`，实现已改为 `image_url.png` / `"image_url fetch failed"` |
| 需要外部资产文件 | 5 | 读 `assets/image.png`、`assets/chery_studio.png`，仓库里只有 `atlascloud.svg` |
| 需要活服务在 8000 端口 | 6 | 8000 被 `/home/Register` 的进程占着，不是本项目 |
| 测试间状态污染 | 4 | `test_image_tasks_api.py` 单独跑 4 passed，全量跑 401（前面的测试改了 auth 状态） |
| 上游/协议行为变更 | 1 | `test_multi_image_results.py` 期望两个 file_id，实现只返回一个 |

结论：**Python 测试套件不能当等价性契约**，它自身就有 24 个已知偏差。golden 快照只录「当前实际行为」，不录「测试期望行为」——这是两者唯一的区别，也是最容易被搞错的地方。

## Consequences

- 录制脚本本身要能复现，不能手工整理；它读的是跑起来的 Python 服务，不是测试文件。
- 某个 golden 若反映的是明显 bug（如 `download_images_zip` 的 WebDAV 分支被覆盖），Go 版按**修复后**行为实现，并在该条 golden 上显式标注 `intentional-divergence`，不允许静默改。
