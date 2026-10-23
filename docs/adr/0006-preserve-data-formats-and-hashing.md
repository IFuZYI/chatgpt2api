# 存量数据格式与认证哈希原样保留，不做迁移

`data/` 下的文件是用户的真实资产：`accounts.json`（号池）、`auth_keys.json`（密钥）、`logs.jsonl`（调用日志）、`image_tasks.json`、`editable_file_tasks.json`、`image_tags.json`、`image_index.json`、`cpa_config.json`、`sub2api_config.json`、`files/`、`images/`。切换实现时不做任何格式迁移——Go 版直接读写同一批文件，字段名、嵌套、时间格式（`2006-01-02 15:04:05` 本地时间）全部照旧。

认证哈希同理：`auth_service._hash_key` 是**裸 SHA-256 hex，无盐无迭代**。这是弱哈希，但换成 bcrypt/argon2 会让所有存量 `auth_keys.json` 立即失效，用户被锁在门外。决定保留裸 SHA-256，用 `hmac.Equal` 做常数时间比较（对齐 Python 的 `hmac.compare_digest`）。

## Consequences

- 账号对象有 20+ 个字段（`_normalize_account` 的完整输出就是 schema），Go 结构体必须全部覆盖并用指针/可选类型区分"缺失"与"零值"，否则回写会把 `null` 写成 `""`，污染用户数据。
- `logs.jsonl` 是 append-only 单行 JSON，`id` 为 uuid4 hex、`detail` 为任意对象；Go 版继续用 `map[string]any` 存 `detail`，不用强类型。
- 想升级哈希需要一次性迁移（下次登录时 rehash），那是独立议题，不在本次重构范围。
- 若将来引入 `data/` 版本号，必须能读旧格式；本次不引入。
