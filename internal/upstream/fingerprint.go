package upstream

import (
	"fmt"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// BrowserProfile 是一个浏览器身份：TLS profile + 默认 UA + sec-ch-ua。
type BrowserProfile struct {
	TLSProfile profiles.ClientProfile
	UserAgent  string
	SecChUa    string
	// Name 是映射表里的键，仅用于日志与错误信息。
	Name string
}

// Chrome 系默认 UA 模板。账号里存的 fp.user-agent 优先于它。
const (
	defaultChromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"
	defaultSecChUa = `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`
)

// 指纹映射表 —— 见 docs/adr/0002-upstream-tls-fingerprint.md。
//
// Python 版用 curl_cffi 的 impersonate 字符串（chrome110 / edge101 / chrome）。
// tls-client 没有 edge profile（已枚举全部：chrome_103..chrome_152 / firefox_* /
// opera_* / brave_146），所以 edge 必须映射到同代 Chromium。
//
// ⚠️ 这张表是**实测**出来的，不是按版本号猜的。对 https://chatgpt.com/ 各测 5 次：
//
//	profile     结果
//	chrome_103  200
//	chrome_110  200  (5/5)
//	chrome_117  200
//	chrome_120  403 challenge  ← 不能用
//	chrome_124  403 challenge  ← 不能用（加不发 header 都一样）
//	chrome_131  200  (5/5)
//	chrome_133  403 challenge  ← 不能用
//	chrome_144  200  (5/5)
//	chrome_146  200  (5/5)
//	chrome_150  200  (5/5)
//	chrome_152  200  (5/5)
//	firefox_135 200
//	safari_16_0 200
//
// 另外：完全不带 UA/sec-ch-ua 时，连 chrome_150 也拿 403。所以
// 「TLS profile 与 header 必须成对出现」不是建议，是硬约束。
//
// 结论：映射只落已验证通过的档位，未命中一律 Chrome_150（tls-client 的
// DefaultClientProfile，也是实测通过的最新档）。
var profileTable = map[string]BrowserProfile{
	"chrome110": {Name: "chrome110", TLSProfile: profiles.Chrome_110, UserAgent: defaultChromeUA, SecChUa: defaultSecChUa},
	"chrome117": {Name: "chrome117", TLSProfile: profiles.Chrome_117, UserAgent: defaultChromeUA, SecChUa: defaultSecChUa},
	"chrome131": {Name: "chrome131", TLSProfile: profiles.Chrome_131, UserAgent: defaultChromeUA, SecChUa: defaultSecChUa},
	"chrome144": {Name: "chrome144", TLSProfile: profiles.Chrome_144, UserAgent: defaultChromeUA, SecChUa: defaultSecChUa},
	"chrome146": {Name: "chrome146", TLSProfile: profiles.Chrome_146, UserAgent: defaultChromeUA, SecChUa: defaultSecChUa},
	"chrome150": {Name: "chrome150", TLSProfile: profiles.Chrome_150, UserAgent: defaultChromeUA, SecChUa: defaultSecChUa},
	"chrome152": {Name: "chrome152", TLSProfile: profiles.Chrome_152, UserAgent: defaultChromeUA, SecChUa: defaultSecChUa},
	// Edge 143 与 Chromium 143 同源；143 无独立 profile，用最近的已验证档 144。
	"edge101": {Name: "edge101->chrome144", TLSProfile: profiles.Chrome_144, UserAgent: defaultChromeUA, SecChUa: defaultSecChUa},
	// 不带版本号的 "chrome" 用 tls-client 的默认档（也是实测通过的最新档）。
	"chrome":     {Name: "chrome->chrome150", TLSProfile: profiles.Chrome_150, UserAgent: defaultChromeUA, SecChUa: defaultSecChUa},
	"firefox135": {Name: "firefox135", TLSProfile: profiles.Firefox_135},
}

// defaultProfile 是未命中映射表时的兜底。用实测通过且最新的 Chrome_150，
// 不用 Chrome_124 —— 后者对 Cloudflare 稳定返回 403 challenge。
var defaultProfile = BrowserProfile{
	Name:       "chrome150(default)",
	TLSProfile: profiles.Chrome_150,
	UserAgent:  defaultChromeUA,
	SecChUa:    defaultSecChUa,
}

// ResolveProfile 把账号里存的 impersonate 字符串解析成具体 profile。
func ResolveProfile(impersonate string) BrowserProfile {
	key := strings.ToLower(strings.TrimSpace(impersonate))
	if key == "" {
		return defaultProfile
	}
	if profile, ok := profileTable[key]; ok {
		return profile
	}
	return defaultProfile
}

// headerOrder 复刻 Python 版 OpenAIBackendAPI.__init__ 里 session.headers 的插入顺序。
//
// 顺序是 Cloudflare 判定输入的一部分，不能按字母序重排（见 ADR-0002）。
var headerOrder = []string{
	"user-agent",
	"origin",
	"referer",
	"accept-language",
	"cache-control",
	"pragma",
	"priority",
	"sec-ch-ua",
	"sec-ch-ua-arch",
	"sec-ch-ua-bitness",
	"sec-ch-ua-full-version",
	"sec-ch-ua-full-version-list",
	"sec-ch-ua-mobile",
	"sec-ch-ua-model",
	"sec-ch-ua-platform",
	"sec-ch-ua-platform-version",
	"sec-fetch-dest",
	"sec-fetch-mode",
	"sec-fetch-site",
	"oai-device-id",
	"oai-session-id",
	"oai-language",
	"oai-client-version",
	"oai-client-build-number",
	"authorization",
}

// ClientOptions 是构造一个上游客户端的全部输入。
type ClientOptions struct {
	Impersonate string
	ProxyURL    string
	SkipVerify  bool
	TimeoutSec  int
}

// newClientImpl 构造一个带指纹的上游 HTTP 客户端。
//
// 这里刻意返回 tls_client.HttpClient 而不是 *http.Client：调用方必须用
// fhttp 的 Request（才能控制 HeaderOrderKey），混用标准库 request 会丢掉指纹。
func newClientImpl(opts ClientOptions) (tls_client.HttpClient, BrowserProfile, error) {
	profile := ResolveProfile(opts.Impersonate)

	clientOptions := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(profile.TLSProfile),
		// 与 curl_cffi 的 timeout 语义对齐：不设超时时长由调用方 ctx 控制。
		tls_client.WithTimeoutSeconds(opts.TimeoutSec),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
	}

	// 协议竞速（HTTP/3 QUIC + HTTP/2）**只在 SOCKS5 代理下启用**。
	//
	// 两个事实必须同时成立：
	//  1. 图片链路 /backend-api/f/conversation 在纯 HTTP/2 下会被上游 500，
	//     需要 HTTP/3 才能通（实测，见 docs/adr 相关记录）。
	//  2. tls-client v1.17.1 的 protocol racing 要求 SOCKS5 代理：
	//     HTTP/HTTPS 代理只能隧道 TCP，无法承载 QUIC/UDP，会绕过代理泄漏真实 IP。
	//     传 http:// 或 https:// 代理 + racing 会在 NewHttpClient 阶段直接报错：
	//     "protocol racing requires a SOCKS5 proxy for HTTP/3 support"。
	//
	// 早期实现无条件启用 racing，导致所有 HTTP/HTTPS 代理用户连客户端都建不起来
	// （实测仓库默认的 http://privoxy:8118 就是这种配置）。这里按代理 scheme 分流：
	// 直连或 socks5/socks5h 才开 racing；HTTP(S) 代理则退回纯 HTTP/2。
	if opts.ProxyURL != "" {
		clientOptions = append(clientOptions, tls_client.WithProxyUrl(opts.ProxyURL))
	}
	if supportsProtocolRacing(opts.ProxyURL) {
		clientOptions = append(clientOptions, tls_client.WithProtocolRacing())
	}
	if opts.SkipVerify {
		clientOptions = append(clientOptions, tls_client.WithInsecureSkipVerify())
	}

	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), clientOptions...)
	if err != nil {
		return nil, profile, fmt.Errorf("build tls client (%s): %w", profile.Name, err)
	}
	return client, profile, nil
}

// BuildHeaders 按 Python 版的字段集与顺序构造请求头。
//
// extra 里的键会覆盖同名默认头；随后把 headerOrder 里出现的键写进
// fhttp.HeaderOrderKey，保证线上顺序稳定。
func BuildHeaders(profile BrowserProfile, deviceID, sessionID, clientVersion, clientBuild, accessToken string, extra map[string]string) http.Header {
	headers := http.Header{}

	set := func(name, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		headers[strings.ToLower(name)] = []string{value}
	}

	set("User-Agent", profile.UserAgent)
	set("Origin", "https://chatgpt.com")
	set("Referer", "https://chatgpt.com/")
	set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7")
	set("Cache-Control", "no-cache")
	set("Pragma", "no-cache")
	set("Priority", "u=1, i")
	set("Sec-Ch-Ua", profile.SecChUa)
	set("Sec-Ch-Ua-Arch", `"x86"`)
	set("Sec-Ch-Ua-Bitness", `"64"`)
	set("Sec-Ch-Ua-Full-Version", `"143.0.3650.96"`)
	set("Sec-Ch-Ua-Full-Version-List", `"Microsoft Edge";v="143.0.3650.96", "Chromium";v="143.0.7499.147", "Not A(Brand";v="24.0.0.0"`)
	set("Sec-Ch-Ua-Mobile", "?0")
	set("Sec-Ch-Ua-Model", `""`)
	set("Sec-Ch-Ua-Platform", `"Windows"`)
	set("Sec-Ch-Ua-Platform-Version", `"19.0.0"`)
	set("Sec-Fetch-Dest", "empty")
	set("Sec-Fetch-Mode", "cors")
	set("Sec-Fetch-Site", "same-origin")
	set("OAI-Device-Id", deviceID)
	set("OAI-Session-Id", sessionID)
	set("OAI-Language", "zh-CN")
	set("OAI-Client-Version", clientVersion)
	set("OAI-Client-Build-Number", clientBuild)
	// 只在有 token 时才加 Authorization。空 token 时发出 "Bearer "（尾部空格）
	// 会被上游判为无效凭据，匿名链路直接 500 —— 这是实测踩到的坑。
	if strings.TrimSpace(accessToken) != "" {
		set("Authorization", "Bearer "+accessToken)
	}

	for name, value := range extra {
		set(name, value)
	}

	order := make([]string, 0, len(headerOrder)+len(extra))
	seen := map[string]bool{}
	for _, name := range headerOrder {
		if _, ok := headers[name]; ok {
			order = append(order, name)
			seen[name] = true
		}
	}
	// extra 里多出来的键按字母序追加，保证可复现。
	extras := make([]string, 0, len(extra))
	for name := range extra {
		lowered := strings.ToLower(name)
		if seen[lowered] {
			continue
		}
		if _, ok := headers[lowered]; ok {
			extras = append(extras, lowered)
		}
	}
	sortStrings(extras)
	order = append(order, extras...)

	headers[http.HeaderOrderKey] = order
	return headers
}

// supportsProtocolRacing 报告当前代理配置能否安全启用 HTTP/3 协议竞速。
//
// 规则：无代理（直连）或 SOCKS5/SOCKS5H 可以；HTTP/HTTPS 代理不行
// （无法隧道 UDP，tls-client 会在建客户端时直接拒绝）。
func supportsProtocolRacing(proxyURL string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(proxyURL))
	if trimmed == "" {
		return true
	}
	return strings.HasPrefix(trimmed, "socks5://") || strings.HasPrefix(trimmed, "socks5h://")
}
