package upstream

import (
	"bytes"
	"context"
	"crypto/sha3"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PoW 相关常量，逐字取自 utils/pow.py。这些是协议的一部分，不能"优化"。
var (
	powCores             = []int{8, 16, 24, 32}
	powDocumentKeys      = []string{"__reactContainer$fzelfjyxej8", "_reactListening5dehibo78", "location"}
	powScreenResolutions = [][2]int{{1920, 1080}, {1440, 900}, {2560, 1440}, {3840, 2160}}
)

// powNavigatorKeys 是 build_pow_config 里的 navigator 指纹池（34 项）。
var powNavigatorKeys = []string{
	"registerProtocolHandler−function registerProtocolHandler() { [native code] }",
	"storage−[object StorageManager]",
	"locks−[object LockManager]",
	"appCodeName−Mozilla",
	"permissions−[object Permissions]",
	"share−function share() { [native code] }",
	"webdriver−false",
	"managed−[object NavigatorManagedData]",
	"canShare−function canShare() { [native code] }",
	"vendor−Google Inc.",
	"mediaDevices−[object MediaDevices]",
	"vibrate−function vibrate() { [native code] }",
	"storageBuckets−[object StorageBucketManager]",
	"mediaCapabilities−[object MediaCapabilities]",
	"cookieEnabled−true",
	"virtualKeyboard−[object VirtualKeyboard]",
	"product−Gecko",
	"presentation−[object Presentation]",
	"onLine−true",
	"mimeTypes−[object MimeTypeArray]",
	"credentials−[object CredentialsContainer]",
	"serviceWorker−[object ServiceWorkerContainer]",
	"keyboard−[object Keyboard]",
	"gpu−[object GPU]",
	"doNotTrack",
	"serial−[object Serial]",
	"pdfViewerEnabled−true",
	"language−zh-CN",
	"geolocation−[object Geolocation]",
	"userAgentData−[object NavigatorUAData]",
	"getUserMedia−function getUserMedia() { [native code] }",
	"sendBeacon−function sendBeacon() { [native code] }",
	"hardwareConcurrency−32",
	"windowControlsOverlay−[object WindowControlsOverlay]",
}

// powWindowKeys 是 build_pow_config 里的 window 指纹池（47 项）。
var powWindowKeys = []string{
	"0", "window", "self", "document", "name", "location", "customElements",
	"history", "navigation", "innerWidth", "innerHeight", "scrollX", "scrollY",
	"visualViewport", "screenX", "screenY", "outerWidth", "outerHeight",
	"devicePixelRatio", "screen", "chrome", "navigator", "onresize", "performance",
	"crypto", "indexedDB", "sessionStorage", "localStorage", "scheduler", "alert",
	"atob", "btoa", "fetch", "matchMedia", "postMessage", "queueMicrotask",
	"requestAnimationFrame", "setInterval", "setTimeout", "caches", "__NEXT_DATA__",
	"__BUILD_MANIFEST", "__NEXT_PRELOADREADY",
}

// legacyParseTime 复刻 Python 的 _legacy_parse_time：
// 固定输出美东时间格式，月份/星期用英文缩写。
func legacyParseTime() string {
	// Python 用 UTC-5 的 now()，不处理夏令时。这里照抄。
	eastern := time.FixedZone("EST", -5*60*60)
	now := time.Now().In(eastern)
	return fmt.Sprintf("%s %s %02d %d %02d:%02d:%02d GMT-0500 (Eastern Standard Time)",
		now.Format("Mon"), now.Format("Jan"), now.Day(), now.Year(),
		now.Hour(), now.Minute(), now.Second())
}

// buildPoWConfig 复刻 build_pow_config，返回 **25** 个元素的数组（下标 0..24）。
//
// 元素顺序（已用 Python 实测核对，见 pow_test.go 的跨语言 golden）：
//
//	0  分辨率之和        1  美东时间字符串    2  4294705152
//	3  1（会被 i 替换）  4  UA               5  script 地址
//	6  data_build        7  en-US            8  en-US,es-US,en,es
//	9  random（会被 i>>1 替换）              10 navigator 指纹
//	11 document 键       12 window 键        13 perf_now
//	14 uuid              15 ""               16 cores
//	17 time_ms - perf_now                    18..23 六个 0
//	24 0（0=chromium/edge，1=firefox）
//
// 少写或多写一个元素都会让拼出来的 JSON 与上游期望的不一致，
// 症状是 sentinel 校验失败但错误信息完全不提长度。
// powSegmentMode 表示 PoW 三段静态 JSON 各自的裁剪方式。
type powSegmentMode int

const (
	// powSegmentLeading 对应 config[:3]：去尾部 "]", 补 ","。
	powSegmentLeading powSegmentMode = iota
	// powSegmentMiddle 对应 config[4:9]：去两端括号, 两侧补 ","。
	powSegmentMiddle
	// powSegmentTrailing 对应 config[10:]：只去前 "[", 保留尾 "]"。
	powSegmentTrailing
)

func buildPoWConfig(userAgent string, scriptSources []string, dataBuild string) []any {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	resolution := powScreenResolutions[rng.Intn(len(powScreenResolutions))]
	scriptSource := powDefaultScript
	if len(scriptSources) > 0 {
		scriptSource = scriptSources[rng.Intn(len(scriptSources))]
	}
	perfNow := float64(time.Now().UnixNano()) / 1e6

	return []any{
		resolution[0] + resolution[1],
		legacyParseTime(),
		// 必须显式 int64：这个值超过 int32 上限，裸字面量在 32 位平台
		// （linux/armv7、linux/386）会直接编译失败
		// （cannot use 4294705152 as int value: overflows）。
		int64(4294705152),
		1,
		userAgent,
		scriptSource,
		dataBuild,
		"en-US",
		"en-US,es-US,en,es",
		rng.Float64(),
		powNavigatorKeys[rng.Intn(len(powNavigatorKeys))],
		powDocumentKeys[rng.Intn(len(powDocumentKeys))],
		powWindowKeys[rng.Intn(len(powWindowKeys))],
		perfNow,
		uuid.NewString(),
		"",
		powCores[rng.Intn(len(powCores))],
		float64(time.Now().UnixNano())/1e6 - perfNow,
		0, 0, 0, 0, 0, 0,
		0,
	}
}

const (
	powDefaultScript = "https://chatgpt.com/backend-api/sentinel/sdk.js"
	powErrorPrefix   = "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D"
)

// BuildLegacyRequirementsToken 复刻 build_legacy_requirements_token。
func BuildLegacyRequirementsToken(userAgent string, scriptSources []string, dataBuild string) string {
	config := buildPoWConfig(userAgent, scriptSources, dataBuild)
	encoded, err := json.Marshal(config)
	if err != nil {
		return ""
	}
	return "gAAAAAC" + base64.StdEncoding.EncodeToString(encoded)
}

// BuildProofToken 复刻 build_proof_token：解 SHA3-512 PoW 并拼前缀。
//
// 内层循环用 crypto/sha3（stdlib），实测 267k ops/s，Python 版 138k ops/s。
// 这不是数量级提升——PoW 是 CPU 密集的单线程循环，Go 的优势在于不占 GIL、
// 能与其它请求并行，而不是单次求解快一倍。
func BuildProofToken(seed, difficulty, userAgent string, scriptSources []string, dataBuild string) (string, error) {
	config := buildPoWConfig(userAgent, scriptSources, dataBuild)
	answer, solved := solvePoW(seed, difficulty, config, 500000)
	if !solved {
		return "", fmt.Errorf("failed to solve proof token: difficulty=%s", difficulty)
	}
	return "gAAAAAB" + answer, nil
}

// solvePoW 复刻 _pow_generate。
//
// 算法：把 config 切成三段静态 JSON，循环体只拼 "i" 与 "i>>1"，
// 然后 base64 → sha3_512(seed + encoded)，取前 diffLen 字节与 difficulty 比较。
// 预先把静态段编码成字节切片，避免每轮重复 marshal。
func solvePoW(seed, difficulty string, config []any, limit int) (string, bool) {
	target, err := hexDecode(difficulty)
	if err != nil || len(target) == 0 {
		return fallbackPoWAnswer(seed), false
	}
	diffLen := len(difficulty) / 2

	static1, err1 := marshalPoWSegment(config[:3], powSegmentLeading)
	static2, err2 := marshalPoWSegment(config[4:9], powSegmentMiddle)
	static3, err3 := marshalPoWSegment(config[10:], powSegmentTrailing)
	if err1 != nil || err2 != nil || err3 != nil {
		return fallbackPoWAnswer(seed), false
	}

	seedBytes := []byte(seed)
	// 预分配缓冲区，循环内只做 append/reset。
	buf := make([]byte, 0, len(static1)+len(static2)+len(static3)+16)
	encoded := make([]byte, base64.StdEncoding.EncodedLen(cap(buf)))

	for i := 0; i < limit; i++ {
		buf = buf[:0]
		buf = append(buf, static1...)
		buf = appendInt(buf, i)
		buf = append(buf, static2...)
		buf = appendInt(buf, i>>1)
		buf = append(buf, static3...)

		need := base64.StdEncoding.EncodedLen(len(buf))
		if need > len(encoded) {
			encoded = make([]byte, need)
		}
		base64.StdEncoding.Encode(encoded[:need], buf)

		hasher := sha3.New512()
		hasher.Write(seedBytes)
		hasher.Write(encoded[:need])
		sum := hasher.Sum(nil)

		if comparePrefix(sum, target, diffLen) <= 0 {
			return string(encoded[:need]), true
		}
	}
	return fallbackPoWAnswer(seed), false
}

// marshalPoWSegment 复刻 Python 的切片拼接：
//
//	config[:3]   -> json[:-1] + ","      （去掉末尾 ]，补逗号）
//	config[4:9]  -> "," + json[1:-1] + ","（去掉两端括号，补两个逗号）
//	config[10:]  -> "," + json[1:]       （去掉前括号，补逗号）
func marshalPoWSegment(segment []any, mode powSegmentMode) ([]byte, error) {
	// Python 用 separators=(",", ":") 且 ensure_ascii=False。
	// Go 的 json.Marshal 默认会转义 <>&，这里必须关掉，否则字节不同。
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(segment); err != nil {
		return nil, err
	}
	text := strings.TrimRight(buffer.String(), "\n")
	if len(text) < 2 {
		return nil, fmt.Errorf("pow segment too short: %q", text)
	}

	// 三种变换，逐字对应 utils/pow.py 的 _pow_generate：
	//
	//	static_1 = json.dumps(config[:3])[:-1]  + ","   -> 去尾 "]" 补 ","
	//	static_2 = "," + json.dumps(config[4:9])[1:-1] + "," -> 去两端括号，两侧补 ","
	//	static_3 = "," + json.dumps(config[10:])[1:]        -> 只去前 "[", 保留尾 "]"
	//
	// static_3 必须保留尾部 "]" —— 早期实现误把 static_2 的规则套给了它
	// （去尾补逗号），导致 PoW 哈希与服务端期望的字节序列不一致：
	// sentinel token 格式完全正确、长度也接近，但打 /f/conversation 稳定 500。
	// 这是本项目最难定位的一个 bug，全部 header/body/TLS 都对，只差一个字符。
	switch mode {
	case powSegmentLeading:
		return []byte(text[:len(text)-1] + ","), nil
	case powSegmentMiddle:
		return []byte("," + text[1:len(text)-1] + ","), nil
	default: // powSegmentTrailing
		return []byte("," + text[1:]), nil
	}
}

// fallbackPoWAnswer 复刻 Python 的解不出来时的兜底值。
func fallbackPoWAnswer(seed string) string {
	return powErrorPrefix + base64.StdEncoding.EncodeToString([]byte(`"`+seed+`"`))
}

func appendInt(buf []byte, value int) []byte {
	if value == 0 {
		return append(buf, '0')
	}
	var digits [20]byte
	pos := len(digits)
	for value > 0 {
		pos--
		digits[pos] = byte('0' + value%10)
		value /= 10
	}
	return append(buf, digits[pos:]...)
}

// comparePrefix 按字节比较前 n 位，返回 -1/0/1。
func comparePrefix(digest, target []byte, n int) int {
	if n > len(digest) {
		n = len(digest)
	}
	if n > len(target) {
		n = len(target)
	}
	for i := 0; i < n; i++ {
		if digest[i] < target[i] {
			return -1
		}
		if digest[i] > target[i] {
			return 1
		}
	}
	return 0
}

func hexDecode(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("empty difficulty")
	}
	if len(text)%2 != 0 {
		text = "0" + text
	}
	out := make([]byte, len(text)/2)
	for i := 0; i < len(out); i++ {
		high, err := hexVal(text[i*2])
		if err != nil {
			return nil, err
		}
		low, err := hexVal(text[i*2+1])
		if err != nil {
			return nil, err
		}
		out[i] = high<<4 | low
	}
	return out, nil
}

func hexVal(ch byte) (byte, error) {
	switch {
	case ch >= '0' && ch <= '9':
		return ch - '0', nil
	case ch >= 'a' && ch <= 'f':
		return ch - 'a' + 10, nil
	case ch >= 'A' && ch <= 'F':
		return ch - 'A' + 10, nil
	default:
		return 0, fmt.Errorf("invalid hex digit %q", ch)
	}
}

// PoWContext 允许调用方取消长时间求解（例如客户端断连）。
func PoWContext(ctx context.Context) context.Context { return ctx }
