# 上游指纹用 tls-client，映射表以实测为准

所有上游请求经 `chatgpt.com`，Python 版靠 `curl_cffi` 的 `impersonate`（`chrome110` / `edge101` / `chrome`）通过 Cloudflare。Go 标准库 `crypto/tls` 的 ClientHello 特征固定，实测对 `https://chatgpt.com/` 101ms 直接返回 403 挑战页——这不是可调优的性能问题，是身份被拒。

选择 `github.com/bogdanfinn/tls-client`：它提供维护中的浏览器 profile，且配套 `fhttp` 允许控制 header 顺序（header 顺序与 TLS 指纹同为 Cloudflare 判定输入）。**它没有 edge profile**（已枚举全部：`chrome_103..chrome_152`、`firefox_*`、`opera_*`、`brave_146`），所以 edge 必须映射到同代 Chromium。

## 映射表是实测出来的，不是按版本号推的

最初按"同代即可"写了 `chrome110→Chrome110`、`edge101→Chrome124`、兜底 `Chrome124`。**这个假设是错的**，实测推翻了它。对 `https://chatgpt.com/` 每个 profile 测 5 次：

| profile | 结果 |
|:--|:--|
| chrome_103 | 200 |
| chrome_110 | 200 (5/5) |
| chrome_117 | 200 |
| **chrome_120** | **403 challenge** |
| **chrome_124** | **403 challenge** |
| chrome_131 | 200 (5/5) |
| **chrome_133** | **403 challenge** |
| chrome_144 | 200 (5/5) |
| chrome_146 | 200 (5/5) |
| chrome_150 | 200 (5/5) |
| chrome_152 | 200 (5/5) |
| firefox_135 | 200 |
| safari_16_0 | 200 |

失败与 UA 无关：把 UA 对齐到 profile 自身版本（`chrome_124` + UA124）仍然是 403，带上完整 sec-ch-ua 也是 403。所以不是"UA 与 TLS 版本不匹配"导致的，是这三个 profile 的 ClientHello 字节特征本身已被 Cloudflare 标记。

**并且**：完全不带 UA / sec-ch-ua 时，连 `chrome_150` 也拿 403。所以「TLS profile 与 header 必须成对出现」是硬约束，不是建议。

## 决定

映射只落已验证通过的档位；未命中一律 `Chrome_150`（tls-client 的 `DefaultClientProfile`，也是实测通过的最新档）。`edge101` 映射到 `Chrome_144`（Edge 143 与 Chromium 143 同源，143 无独立 profile，取最近的已验证档）。

## Consequences

- 这张表**不能靠"升到最新版"维护**。升级 tls-client 后必须重跑一次真实上游探测，因为某个 profile 的字节特征可能刚被 Cloudflare 标记。`internal/upstream` 里有一条对着真实上游的冒烟检查（`-tags live`），不是单测能替代的。
- 每次改 profile 都要同时改 header 集，二者分离会静默失效。
- 未选 `refraction-networking/utls` 手写 ClientHello：控制力更强但要手写每个扩展的字节序，且同样要面对"哪个特征被标记"的黑盒问题，换不来收益。
