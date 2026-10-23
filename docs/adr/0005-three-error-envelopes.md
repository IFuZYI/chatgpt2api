# 三套错误信封逐字节保留，不做统一

同一份代码对外返回三种互不兼容的错误形状，这是已发布的契约，不是历史遗留的偶然：

| 面 | 形状 |
|:--|:--|
| 管理面 `/api/*`、`/auth/*` | `{"detail": {"error": "..."}}`，校验失败时 `{"detail": [{"type","loc","msg","input"}]}` |
| OpenAI 兼容 `/v1/*` | `{"error": {"message","type","param","code"}}` |
| Anthropic `/v1/messages` | `{"type": "error", "error": {"type","message"}}` |

统一成一套是最"干净"的做法，但会同时打断三类客户端：Cherry Studio / New API 这类 OpenAI 兼容客户端按第二套解析、Anthropic SDK 按第三套解析、自带前端按第一套解析。这些解析行为不在本仓库里，改了没法灰度。

决定：三套全部逐字节保留，包括 `type`/`code` 的推导规则（401→`authentication_error`/`invalid_api_key`，403→`permission_error`/`permission_denied`，429→`rate_limit_error`/`rate_limit_exceeded`，4xx→`invalid_request_error`/`bad_request`，5xx→`server_error`/`upstream_error`）和 Pydantic 校验错误的 `loc`/`type`/`msg`/`input` 字段。Go 版手写这套推导，不引入校验库的默认文案。

## Consequences

- 422 的 body 必须逐字段复刻 Pydantic 的措辞（`"Input should be a valid list"`、`"Field required"`、`"Input should be greater than or equal to 5"` 带 `ctx.ge`），这不是可以用 Go validator 库凑出来的，得写一层薄适配。
- 每个新路由必须先确定自己属于哪一套信封，再写 handler。
- golden 基线（ADR-0003）对这三套的覆盖是硬要求，缺一条就等于没验。
