# 匿名对话链路当前不可用，这不是实现缺陷

## 现象（2026-10-23 实测）

`POST /backend-anon/conversation` 的表现：

| 实现 | 状态码 | body |
|:--|:--|:--|
| Python 版（curl_cffi `chrome110`） | **403**，5/5 次稳定 | 空 |
| Go 版（tls-client `chrome_110`） | **500**，多次稳定 | `{"detail":"Hmm...something seems to have gone wrong."}` |

同一时刻两边都能正常完成 sentinel 握手（`token_len≈3100`、`proof_len≈670`），
bootstrap 也正常。所以不是握手问题，是对话端点本身。

## 已排除的原因

逐项验证过，都不是：

- **payload 不一致**：Go 的 `ConversationPayload` 与 Python 的 `_conversation_payload`
  逐字段 JSON 相等（`TestLivePayloadParity` 断言，忽略 `parent_message_id` /
  `websocket_request_id` / `messages` 三个含随机 id 的字段）。
- **header 不一致**：抓了 Python 的完整 header 集合做逐项对比。Go 侧确实多过一个
  `authorization: Bearer `（空 token 时拼出尾部空格），已修。修完仍是 500。
- **某个 header 触发风控**：对 `sec-ch-ua-*`、`oai-*`、`sec-fetch-*`、`priority`、
  `x-openai-target-*` 分别做删除实验，全部仍是 500；退到只剩
  `user-agent + accept + content-type + sentinel 三个 token` 也仍是 500。
- **TLS profile 太老或太新**：`chrome110/131/146/150/152` 全部 500。期间
  `chrome144` 的 bootstrap 还开始返回 403 —— 说明 profile 有效性本身在漂移
  （见 ADR-0002 的实测表）。

## 结论

两边的**状态码不同**（403 vs 500）本身就是证据：Go 的请求穿过了 Cloudflare
（否则会像 Python 一样拿 403），到达应用层后被拒。也就是说 Go 侧的指纹更"干净"，
但匿名对话这个功能被上游按其它维度限制了（很可能与 IP / 未登录身份有关）。

这不是本次重构引入的问题，也无法在客户端侧修好。

## 影响与对策

- **主链路（登录态 `/backend-api/conversation`）不受影响**：账号池里的 access_token
  走的是另一条路径，需要真实账号才能验证。M3 的协议层逻辑（SSE 解析、增量拼接、
  sanitize、file_id 提取）已用 17 个单测覆盖，与链路无关。
- **`/v1/chat/completions` 的匿名回退**：Python 版在号池为空时会用匿名链路，
  这条路径现在两边都不可用。Go 版保持相同行为（尝试 → 失败 → 返回上游错误），
  不额外做"优雅降级"，因为降级成什么都是猜的。
- **live 测试不设成会 fail 的断言**：`TestLiveConversationProbe` 只记录状态码。
  把它写成断言，只会让 live 套件长期红着，而下一次真跑起来时没人再看它。
- **重新评估的触发条件**：等有登录态账号可测时，用 `ClientConfig.AccessToken`
  跑 `TestLiveConversationProbe` 的登录分支；或等上游恢复匿名对话后重跑。

## 这条记录为什么重要

ADR-0002 已经证明「按版本号猜 profile」会错。这条记录是同一类教训的第二次：
**上游可用性是外部变量，不能当成实现的正确性指标**。区分「我们写错了」与
「上游不给」，靠的是 payload/header 的逐字段比对 + 双向状态码差异，而不是
反复重试直到某次成功。
