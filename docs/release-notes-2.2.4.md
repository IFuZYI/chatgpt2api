## ChatGPT2API 2.2.4

图片模型更新版本：新增 ChatGPT Images 2.6 系列。

### 图片模型

- 新增 `gpt-image-2.6`、`gpt-image-2.6-flare`、`gpt-image-2.6-sunburst`。
- 三个模型名直接作为上游 slug 透传，不走 `default_upstream_model_name` 映射。
- `/v1/models` 会按号池能力返回；图片模型仍是内置表，不依赖上游模型目录自动发现。
- 补充模型识别、`SplitImageModel`、上游透传和 `/v1/models` 暴露的回归测试。
- 实测三个模型均能出图（HTTP 200；测试实例使用本地图片存储）。

### 文档

- README / README.en / `docs/feature-status.md` 同步 2.6 系列。
- 不再硬编码文本模型数量，改为说明文本列表随上游与号池变化。

### 验证

- `go test ./internal/protocol/ ./internal/upstream/ ./internal/api/ -count=1`
- `go test -race -count=1 ./internal/protocol/ ./internal/upstream/`
- `go vet ./...`、`gofmt -l internal cmd`
- `cd web && npm test && npm run typecheck`
- `go run ./tools/composecheck`（53 项检查，0 项失败）

---

**完整变更**：见 [CHANGELOG.md](CHANGELOG.md)
