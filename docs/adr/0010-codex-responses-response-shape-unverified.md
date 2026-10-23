# codex 图片链路的响应格式是推断的，请求格式是逐字对齐的

## 背景

`codex-gpt-image-2`（及 `plus-` / `team-` / `pro-` 前缀别名）走的是
`POST /backend-api/codex/responses`，与官网画图的 `/backend-api/f/conversation`
是两套完全独立的协议：

| | web 链路 | codex 链路 |
|:--|:--|:--|
| 端点 | `/backend-api/f/conversation`（先 `prepare` 换 `conduit_token`） | `/backend-api/codex/responses` |
| 请求形态 | 对话消息 + `system_hints=["picture_v2"]` | Responses：`tools` + `tool_choice` |
| 参考图 | 先上传换 `file_id`，用 `file-service://` 指针 | 内联 data URL 进 `input` |
| 结果形态 | SSE 里只有 `file_id`/`sediment_id`，**要再轮询对话文档 + 下载** | 事件里直接带 `image_generation_call.result`（base64） |
| 会话 | 有 `conversation_id`，成功后可按配置删除 | 无会话，无需清理 |

## 决定

**请求侧逐字对齐 Python，响应侧按 Python 的解析器复刻，并把「未验证」写成这里的事实。**

### 请求侧：有权威依据，不需要推断

`services/openai_backend_api.py:786-800` 给出完整 payload，Go 侧
`internal/upstream/codex_image.go:52 codexImagePayload` 逐字段对齐：

```json
{
  "model": "gpt-5.5",
  "instructions": "Use the image_generation tool to create exactly one image for the user's request. Return the generated image result.",
  "store": false,
  "input": [{"role":"user","content":[{"type":"input_text","text":...},{"type":"input_image","image_url":"data:image/png;base64,..."}]}],
  "tools": [{"type":"image_generation","model":"gpt-image-2","action":"edit|generate","size":"1024x1024","quality":"auto","output_format":"png"}],
  "tool_choice": {"type":"image_generation"},
  "stream": true
}
```

三处易错点，都有测试锁定（`internal/upstream/codex_image_test.go`）：

1. `model` 恒为 `gpt-5.5`（`CODEX_RESPONSES_MODEL`），**不是**请求里的
   `codex-gpt-image-2` —— 后者只决定走哪条链路。
2. `tools[0].model` 恒为 `gpt-image-2`（Python 硬编码），不跟随请求模型名。
3. `action` 由「有没有参考图」决定：有图 `edit`、无图 `generate`。

`instructions` 字符串是契约的一部分，包含末尾句号与中间的单空格断行，
改动会导致上游行为漂移。

### 响应侧：只有两处依据，其余是推断

Python 侧能确认的只有：

- `_codex_response_images`（`services/protocol/conversation.py:1245-1260`）：
  递归遍历事件，找 `type == "image_generation_call"` 且 `result` 为字符串的节点，
  若 `result` 以 `data:image/` 开头则按 `split(",", 1)[1]` 剥掉前缀。
- `_iter_codex_response_events`（`openai_backend_api.py:716-775`）：
  响应可能是 SSE（按 `data:` 行累积、空行切事件、跳过 `[DONE]`），
  也可能是单个 `application/json` body（整个 body 当一个事件）。

Go 侧严格复刻这两点（`internal/protocol/codex_image.go:27 codexResponseImages`、
`internal/upstream/codex_image.go` 的 content-type 分流），并额外做**去重**。

## 未验证的点（按风险排序）

1. **真实事件的 `type` 集合**。代码只依赖「任意层级出现
   `type=image_generation_call` 且带 `result`」，不依赖外层是
   `response.output_item.done` 还是 `response.completed` —— 递归查找对两种都成立。
   但如果上游改用别的字段名（例如 `image` 而不是 `result`），会拿不到图。
2. **结果是否分片**。若上游发的是增量事件（如带 `partial_image_b64` 的
   `response.image_generation_call.partial_image`）而不是一次性 `result`，
   当前实现会报 `No image result found in response`。这是最可能的失效模式。
   注意：去重按「完整 base64 内容相等」，分片场景下每个分片都不同，去重不会误伤，
   但也救不了「只有分片、没有完整 result」的情况 —— 那种情况必须改解析逻辑。
3. **`result` 是裸 base64 还是 data URL**。两种都已支持，风险低。
4. **端点是否接受完整浏览器指纹头**。Python 的 `_codex_responses_headers`
   只发 `Authorization` + `Content-Type`；Go 侧用 `c.headers(...)` 带全套
   `sec-ch-ua` / `oai-*` 头（与其它链路一致，见 ADR-0002：TLS 指纹与 header
   必须成对出现）。若上游对该端点做 header 白名单，这是第一个要回退的点。

## 与 Python 的刻意差异

**对重复 base64 去重（跨事件）**。同一个 `image_generation_call` 可能同时出现在
`response.output_item.done` 的 `item` 与 `response.completed` 的
`response.output` 里。Python 的 `_codex_response_images` 每次调用都新建列表，
会把同一张图收两次并返回两张一样的图；Go 侧用 `codexImageCollector`
（`internal/protocol/codex_image.go:22`）在**整个流**上按 base64 内容去重，
只返回一张。这是行为改进，不是兼容性破坏。

去重范围的实现要点：collector 必须存活于整个流（`generateCodexImage` 作用域），
不能是「每解析一个事件就新建」的局部状态 —— 否则跨事件重复照样漏网。
`TestGenerateCodexImageDeduplicatesAcrossEvents`（两个独立事件 → 1 张图）与
`TestGenerateCodexImageKeepsDistinctImages`（两张不同的图 → 2 张）分别锁定了
「不该重复」与「不该误合并」两侧。

其余行为与 Python 一致，包括：

- prompt **不做** web 式改写（不拼「输出图片尺寸为…」，不叠 `global_system_prompt`）。
  尺寸/质量走 `tools[0]`。因此 `global_system_prompt` 对 codex 模型不生效 ——
  这与 Python 的 `stream_codex_image_outputs` 一致，不是遗漏。
- 没有 `conversation_id`，所以异步任务的「继续等待」(resume-poll) 对 codex 任务
  不可用，`image_remove_conversation_*` 清理配置对它也无意义。
- 只有 web 号时，错误是 `no available codex image quota` + **502**（不是 429）：
  Python 的 `raise_image_quota_error` 只匹配字面量 `"no available image quota"`，
  带前缀的文案匹配不上，落到 `ImageGenerationError` 默认的 502。
  `TestCodexModelWithoutCodexAccountReportsQuota` 锁定了这一点。

## 如何补上验证

仓库里已有 `test/test_codex_4k.py`（填 `ACCESS_TOKEN` 后直连上游）。跑一次，
把返回的事件 `type` 列表与 `result` 形态记录下来，回填到
`TestCodexResponseImages*` 与新增的 fixture 里。如果发现是分片形态，
改 `codexResponseImages` 收集 `partial_image_b64` 并按 `item_id` 拼接。

在此之前：**请求侧可以说「与 Python 逐字一致」，响应侧只能说「按 Python 的解析器
复刻」**。任何「codex 出图已端到端验证」的说法都不成立。

## Consequences

- `/v1/models` 继续投放 codex 模型是合理的：链路真实存在，且失败时会给出明确
  错误（`No image result found in response` / 上游状态码），不会静默返回空图。
- 若要临时降级，从 `internal/protocol/models.go` 的 `baseImageModels` 与
  `prefixedCodexImageModels` 移除即可，`/v1/models` 会同步停止投放。
- 真实上游验证后，本 ADR 应更新「未验证的点」一节，而不是删掉它 ——
  记录「哪些是实测、哪些是推断」比记录「现在能用了」更有价值（同 ADR-0008 的教训）。
