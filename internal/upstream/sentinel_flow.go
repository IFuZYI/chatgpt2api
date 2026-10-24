package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"
)

// 本文件逐字移植 utils/sentinel.py。
//
// ⚠️ 它与 pow.go **不是同一套算法**，虽然 token 前缀一样：
//   - pow.go        → SHA3-512 PoW，config 25 项，用于 chat requirements
//   - sentinel.py   → fnv1a_32 PoW，config 18 项，用于 password_verify 等 auth 流程
//
// 两个流程的 config 数组长度、求解算法、难度比较方式都不同，
// 复用 pow.go 会让 sentinel 校验静默失败（token 格式看起来完全正常）。
const (
	// sentinelMaxAttempts 是 Python 里的 MAX_ATTEMPTS。
	sentinelMaxAttempts = 500000
	// sentinelErrorPrefix 与 powErrorPrefix 字面量相同，但属于不同协议，单独定义。
	sentinelErrorPrefix = "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D"
	// sentinelReqURL 是申请 sentinel token 的端点。
	sentinelReqURL = "https://sentinel.openai.com/backend-api/sentinel/req"
	// sentinelScriptURL 是 config[5]，必须与 Python 一致。
	sentinelScriptURL = "https://sentinel.openai.com/sentinel/20260124ceb8/sdk.js"

	sentinelRequirementsPrefix = "gAAAAAC"
	sentinelTokenPrefix        = "gAAAAAB"
	sentinelTokenSuffix        = "~S"
)

// sentinel.py 的默认 UA / sec-ch-ua（与 auth 流程其它请求共用同一份）。
const (
	sentinelDefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) " +
		"Chrome/145.0.0.0 Safari/537.36"
	sentinelDefaultSecChUa = `"Chromium";v="145", "Google Chrome";v="145", "Not/A)Brand";v="99"`
)

// sentinelConfigChoices 是 _get_config 里的三个随机池。
var (
	sentinelNavigatorChoices = []string{
		"vendorSub-undefined",
		"plugins-undefined",
		"mimeTypes-undefined",
		"hardwareConcurrency-undefined",
	}
	sentinelDocumentChoices = []string{"location", "implementation", "URL", "documentURI", "compatMode"}
	sentinelObjectChoices   = []string{"Object", "Function", "Array", "Number", "parseFloat", "undefined"}
	sentinelCoreChoices     = []int{4, 8, 12, 16}
)

// SentinelTokenGenerator 是 sentinel.py 里 SentinelTokenGenerator 的等价物。
type SentinelTokenGenerator struct {
	deviceID  string
	userAgent string
	sid       string
	// maxAttempts 对应 MAX_ATTEMPTS；单独成字段只为让测试能构造「解不出来」的分支，
	// 生产永远用 sentinelMaxAttempts。
	maxAttempts int
}

// NewSentinelTokenGenerator 构造一个生成器（sid 等价于 Python 的 uuid4）。
func NewSentinelTokenGenerator(deviceID, userAgent string) *SentinelTokenGenerator {
	ua := userAgent
	if strings.TrimSpace(ua) == "" {
		ua = sentinelDefaultUserAgent
	}
	return &SentinelTokenGenerator{
		deviceID:    deviceID,
		userAgent:   ua,
		sid:         newUUID(),
		maxAttempts: sentinelMaxAttempts,
	}
}

// fnv1a32 复刻 SentinelTokenGenerator._fnv1a_32：fnv1a 之后再走四轮混合常数，
// 输出 8 位小写十六进制。Python 按**字符**（码点）迭代，所以这里 range rune。
func fnv1a32(text string) string {
	h := uint32(2166136261)
	for _, r := range text {
		h ^= uint32(r)
		h *= 16777619
	}
	h ^= h >> 16
	h *= 2246822507
	h ^= h >> 13
	h *= 3266489909
	h ^= h >> 16
	return fmt.Sprintf("%08x", h)
}

// sentinelTimestamp 复刻
// time.strftime("%a %b %d %Y %H:%M:%S GMT+0000 (Coordinated Universal Time)", time.gmtime())。
func sentinelTimestamp(at time.Time) string {
	return at.UTC().Format("Mon Jan 02 2006 15:04:05") + " GMT+0000 (Coordinated Universal Time)"
}

// sentinelConfig 复刻 _get_config，返回 **18** 个元素的数组（下标 0..17）。
//
// 元素顺序是协议的一部分，不能调整、不能删项：
//
//	0  分辨率            1  UTC 时间字符串     2  4294705152
//	3  1（会被 i 替换）  4  UA                 5  sdk.js 地址
//	6  null              7  null               8  en-US
//	9  random（会被 i 替换）                   10 navigator 指纹
//	11 document 键       12 Object 键          13 perf_now
//	14 sid(uuid4)        15 ""                 16 cores
//	17 time_ms - perf_now
func sentinelConfig(userAgent, sid string) []any {
	perfNow := 1000 + rand.Float64()*(50000-1000)
	return []any{
		"1920x1080",
		sentinelTimestamp(time.Now()),
		int64(4294705152),
		1,
		userAgent,
		sentinelScriptURL,
		nil,
		nil,
		"en-US",
		rand.Float64(),
		sentinelNavigatorChoices[rand.Intn(len(sentinelNavigatorChoices))],
		sentinelDocumentChoices[rand.Intn(len(sentinelDocumentChoices))],
		sentinelObjectChoices[rand.Intn(len(sentinelObjectChoices))],
		perfNow,
		sid,
		"",
		sentinelCoreChoices[rand.Intn(len(sentinelCoreChoices))],
		float64(time.Now().UnixNano())/1e6 - perfNow,
	}
}

// encodeSentinelConfig 复刻 _b64：
// base64(json.dumps(data, separators=(",", ":"), ensure_ascii=False))。
//
// 必须关掉 HTML 转义（Go 默认会把 <>& 写成 \u003c 之类），否则字节与 Python 不同。
func encodeSentinelConfig(data []any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(data); err != nil {
		return "", err
	}
	text := strings.TrimRight(buffer.String(), "\n")
	return base64.StdEncoding.EncodeToString([]byte(text)), nil
}

// GenerateRequirementsToken 复刻 generate_requirements_token：
// data[3]=1、data[9]=round(uniform(5,50))，前缀 gAAAAAC。
func (g *SentinelTokenGenerator) GenerateRequirementsToken() (string, error) {
	data := sentinelConfig(g.userAgent, g.sid)
	data[3] = 1
	// Python: round(random.uniform(5, 50)) —— 取整成 JSON 整数。
	data[9] = int(math.Round(5 + rand.Float64()*(50-5)))
	encoded, err := encodeSentinelConfig(data)
	if err != nil {
		return "", err
	}
	return sentinelRequirementsPrefix + encoded, nil
}

// GenerateToken 复刻 generate_token：逐次尝试 i，直到
// fnv1a32(seed + payload)[:len(difficulty)] <= difficulty（字符串字典序比较）。
//
// 解不出来时返回 "gAAAAAB" + ERROR_PREFIX + base64(`"None"`)，与 Python 的
// self._b64(str(None)) 逐字一致。
func (g *SentinelTokenGenerator) GenerateToken(seed, difficulty string) (string, error) {
	start := time.Now()
	data := sentinelConfig(g.userAgent, g.sid)
	if difficulty == "" {
		difficulty = "0"
	}
	limit := g.maxAttempts
	if limit <= 0 {
		limit = sentinelMaxAttempts
	}
	for i := 0; i < limit; i++ {
		data[3] = i
		data[9] = int(math.Round(time.Since(start).Seconds() * 1000))
		payload, err := encodeSentinelConfig(data)
		if err != nil {
			return "", err
		}
		hash := fnv1a32(seed + payload)
		// Python 是 hash[:len(difficulty)] <= difficulty（字符串比较），
		// 难度串比 8 位哈希还长时切片只到哈希末尾，比较自然失败。
		prefixLen := len(difficulty)
		if prefixLen > len(hash) {
			prefixLen = len(hash)
		}
		if hash[:prefixLen] <= difficulty {
			return sentinelTokenPrefix + payload + sentinelTokenSuffix, nil
		}
	}
	// Python 是 self._b64(str(None))，即 json.dumps("None") -> "\"None\""，
	// 也就是 base64 编码后的 `"None"`（含引号），不是 null。
	return sentinelTokenPrefix + sentinelErrorPrefix + base64.StdEncoding.EncodeToString([]byte(`"None"`)), nil
}

// sentinelRequirementsRequest 是 ② 的请求体：{"p","id","flow"}（键序与 Python 一致）。
type sentinelRequirementsRequest struct {
	P    string `json:"p"`
	ID   string `json:"id"`
	Flow string `json:"flow"`
}

// sentinelTokenPayload 是 openai-sentinel-token 的 JSON 结构。
// 用结构体而不是 map 是为了保住 Python dict 的键序（p,t,c,id,flow）。
type sentinelTokenPayload struct {
	P    string `json:"p"`
	T    string `json:"t"`
	C    string `json:"c"`
	ID   string `json:"id"`
	Flow string `json:"flow"`
}

// BuildSentinelTokenForFlow 复刻 build_sentinel_token。
//
// 返回 (openai-sentinel-token header 值, oai-sc cookie 值)。
// oai-sc = "0" + 服务端 challenge token（不是 p 值）。
func BuildSentinelTokenForFlow(ctx context.Context, client *Client, deviceID, flow string) (string, string, error) {
	if client == nil {
		return "", "", fmt.Errorf("sentinel: nil client")
	}
	generator := NewSentinelTokenGenerator(deviceID, sentinelDefaultUserAgent)
	requirementsToken, err := generator.GenerateRequirementsToken()
	if err != nil {
		return "", "", fmt.Errorf("sentinel: encode requirements token: %w", err)
	}
	// 用结构体保住 Python dict 的键序（p,id,flow），不用 map（Go 会按字母序输出）。
	requestBody, err := json.Marshal(sentinelRequirementsRequest{
		P:    requirementsToken,
		ID:   deviceID,
		Flow: flow,
	})
	if err != nil {
		return "", "", err
	}
	headers := map[string]string{
		"Content-Type":       "text/plain;charset=UTF-8",
		"Referer":            "https://sentinel.openai.com/backend-api/sentinel/frame.html",
		"Origin":             "https://sentinel.openai.com",
		"User-Agent":         sentinelDefaultUserAgent,
		"sec-ch-ua":          sentinelDefaultSecChUa,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
	}
	response, status, err := client.AuthPost(ctx, sentinelReqURL, headers, requestBody, 20*time.Second)
	if err != nil {
		return "", "", fmt.Errorf("sentinel_req_failed_%d: %w", status, err)
	}
	// 非 200 一律报错，不进兜底分支。
	//
	// Python 的 except 只包住 resp.json()，所以遇到 429/400 的 HTML 错误页会
	// 走进 fallback 并返回一个 c="" 的废 token，真正的失败被推迟到
	// password/verify 的 403 —— 现场只剩「密码正确却 403」。实测踩到过：
	// 连发几次探测后上游回 HTML 400，构建函数却"成功"了。
	if status != 200 {
		return "", "", fmt.Errorf("sentinel_req_failed_%d: %s", status, truncate(string(response), 200))
	}

	var data map[string]any
	if len(response) > 0 {
		if unmarshalErr := json.Unmarshal(response, &data); unmarshalErr != nil {
			// 200 但 body 不是 JSON：按 Python 的兜底拼一个空 challenge 的 token，
			// 让调用方自己决定要不要继续（password/verify 会失败，但不会 panic）。
			fallbackP, fallbackErr := generator.GenerateRequirementsToken()
			if fallbackErr != nil {
				return "", "", fallbackErr
			}
			fallback, marshalErr := json.Marshal(sentinelTokenPayload{
				P: fallbackP, ID: deviceID, Flow: flow,
			})
			if marshalErr != nil {
				return "", "", marshalErr
			}
			return string(fallback), "", nil
		}
	}
	if data == nil {
		data = map[string]any{}
	}

	token := strings.TrimSpace(stringValue(data["token"]))
	if token == "" {
		return "", "", fmt.Errorf("sentinel_req_failed_%d: response has no token: %s",
			status, truncate(string(response), 200))
	}

	pValue := ""
	proof, _ := data["proofofwork"].(map[string]any)
	seed := stringValue(proof["seed"])
	if truthy(proof["required"]) && seed != "" {
		solved, solveErr := generator.GenerateToken(seed, stringValue(proof["difficulty"]))
		if solveErr != nil {
			return "", "", solveErr
		}
		pValue = solved
	} else {
		requirements, reqErr := generator.GenerateRequirementsToken()
		if reqErr != nil {
			return "", "", reqErr
		}
		pValue = requirements
	}

	payload, err := json.Marshal(sentinelTokenPayload{
		P:    pValue,
		C:    token,
		ID:   deviceID,
		Flow: flow,
	})
	if err != nil {
		return "", "", err
	}
	return string(payload), "0" + token, nil
}

// BuildSentinelToken 是 *Client 上的便捷入口（供 service 层通过接口调用）。
func (c *Client) BuildSentinelToken(ctx context.Context, deviceID, flow string) (string, string, error) {
	return BuildSentinelTokenForFlow(ctx, c, deviceID, flow)
}
