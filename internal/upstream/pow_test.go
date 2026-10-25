package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// 跨语言 golden：这段 JSON 由 Python 的 build_pow_config 在固定种子下产出
// （random.seed(42)）。它锁住三件事：
//  1. 元素个数是 25（下标 0..24）
//  2. 元素顺序完全一致
//  3. JSON 编码不带 HTML 转义（− 与 $ 原样保留）
//
// 任何一项漂移都会让 sentinel 校验失败，而错误信息不会告诉你原因。
const pythonPoWConfigJSON = `[3000,"Fri Oct 23 2026 01:33:23 GMT-0500 (Eastern Standard Time)",4294705152,1,"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0","https://chatgpt.com/backend-api/sentinel/sdk.js","c/abc/_","en-US","en-US,es-US,en,es",0.1395379285251439,"managed−[object NavigatorManagedData]","__reactContainer$fzelfjyxej8","window",74767621.061866,"7b1ad1f2-b868-49f7-aee0-dc8266cde9e2","",8,1792662436034.927,0,0,0,0,0,0,0]`

func TestPoWConfigShapeMatchesPython(t *testing.T) {
	var reference []any
	if err := json.Unmarshal([]byte(pythonPoWConfigJSON), &reference); err != nil {
		t.Fatalf("reference json invalid: %v", err)
	}
	if len(reference) != 25 {
		t.Fatalf("python reference has %d elements, expected 25", len(reference))
	}

	config := buildPoWConfig(
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0",
		[]string{"https://chatgpt.com/backend-api/sentinel/sdk.js"},
		"c/abc/_",
	)
	if len(config) != 25 {
		t.Fatalf("buildPoWConfig produced %d elements, want 25 (Python produces 25)", len(config))
	}

	// 类型检查：这些位置必须是数字/字符串，顺序错了就会在这里炸。
	typeChecks := map[int]string{
		0: "number", 1: "string", 2: "number", 3: "number", 4: "string",
		5: "string", 6: "string", 7: "string", 8: "string", 9: "number",
		10: "string", 11: "string", 12: "string", 13: "number", 14: "string",
		15: "string", 16: "number", 17: "number", 18: "number", 19: "number",
		20: "number", 21: "number", 22: "number", 23: "number", 24: "number",
	}
	for index, want := range typeChecks {
		got := kindOf(config[index])
		if got != want {
			t.Errorf("element %d is %s, want %s (value=%v)", index, got, want, config[index])
		}
	}

	// 固定值位置必须与 Python 一致。
	//
	// 断言必须写 int64(...)：config[2] 的动态类型是 int64（生产代码显式标注，
	// 因为裸字面量在 32 位平台会溢出 int）。interface 比较连动态类型一起比，
	// 拿无类型常量 4294705152（默认推成 int）去比会永远不相等。
	if config[2] != int64(4294705152) {
		t.Errorf("element 2 = %v (%T), want 4294705152 (int64)", config[2], config[2])
	}
	if config[3] != 1 {
		t.Errorf("element 3 = %v, want 1", config[3])
	}
	if config[7] != "en-US" {
		t.Errorf("element 7 = %v, want en-US", config[7])
	}
	if config[8] != "en-US,es-US,en,es" {
		t.Errorf("element 8 = %v", config[8])
	}
	if config[15] != "" {
		t.Errorf("element 15 = %v, want empty string", config[15])
	}
	if config[24] != 0 {
		t.Errorf("element 24 = %v, want 0 (chromium/edge flag)", config[24])
	}
	// JSON 解出来的数字是 float64，配置里的整数是 int / int64（元素 2 是 int64，
	// 因为它超过 int32 上限，显式标注才能在 32 位平台编译）。比较前统一转 float64。
	if reference[2].(float64) != float64(config[2].(int64)) {
		t.Errorf("element 2 drifts from python reference: %v vs %v", reference[2], config[2])
	}
	if reference[3].(float64) != float64(config[3].(int)) {
		t.Errorf("element 3 drifts from python reference")
	}
	if reference[24].(float64) != float64(config[24].(int)) {
		t.Errorf("element 24 drifts from python reference")
	}
}

func kindOf(value any) string {
	switch value.(type) {
	case string:
		return "string"
	case float64, int, int64:
		return "number"
	case bool:
		return "bool"
	default:
		return "other"
	}
}

// marshalPoWSegment 必须复刻 Python 的 separators=(",", ":") 与 ensure_ascii=False。
func TestPoWSegmentEncodingNoHTMLEscape(t *testing.T) {
	segment := []any{
		3000,
		"managed−[object NavigatorManagedData]",
		"__reactContainer$fzelfjyxej8",
	}
	encoded, err := marshalPoWSegment(segment, powSegmentMiddle)
	if err != nil {
		t.Fatalf("marshalPoWSegment: %v", err)
	}
	text := string(encoded)

	if strings.Contains(text, `\u2212`) {
		t.Error("em-dash was HTML/JSON escaped; Python keeps it literal")
	}
	if !strings.Contains(text, "−") {
		t.Error("em-dash missing from encoded segment")
	}
	if !strings.Contains(text, "$fzelfjyxej8") {
		t.Error("dollar sign was escaped; Python keeps it literal")
	}
	// 无空格分隔（separators 是 "," 与 ":"）。
	if strings.Contains(text, `", "`) || strings.Contains(text, `": "`) {
		t.Errorf("segment has spaces after separators: %s", text)
	}
}

// 三段拼接后必须能还原成合法的 JSON 数组。
func TestPoWSegmentReassembly(t *testing.T) {
	config := buildPoWConfig("UA", nil, "build")

	static1, err := marshalPoWSegment(config[:3], powSegmentLeading)
	if err != nil {
		t.Fatalf("segment1: %v", err)
	}
	static2, err := marshalPoWSegment(config[4:9], powSegmentMiddle)
	if err != nil {
		t.Fatalf("segment2: %v", err)
	}
	static3, err := marshalPoWSegment(config[10:], powSegmentTrailing)
	if err != nil {
		t.Fatalf("segment3: %v", err)
	}

	// Python: static_1 + str(i) + static_2 + str(i>>1) + static_3
	//
	// 三段拼起来就是完整的 25 元素数组（static_3 自带尾部 "]"），
	// 不需要额外补括号。这个断言锁住字节级契约：
	// 早期实现把 static_3 写成 ",... ," 少了尾部 "]"，PoW 哈希与服务端
	// 期望的字节序列不一致，sentinel token 格式正确但上游稳定 500。
	assembled := string(static1) + "0" + string(static2) + "0" + string(static3)

	var decoded []any
	if err := json.Unmarshal([]byte(assembled), &decoded); err != nil {
		t.Fatalf("reassembled json invalid: %v\nassembled=%s", err, assembled)
	}
	if len(decoded) != 25 {
		t.Fatalf("reassembled array has %d elements, want 25", len(decoded))
	}
}

// 求解器必须能解出简单难度，并且在无解时返回 Python 同款兜底值。
func TestSolvePoW(t *testing.T) {
	config := buildPoWConfig("UA", nil, "")
	answer, solved := solvePoW("seed-value", "00", config, 5000)
	if !solved {
		t.Fatalf("failed to solve difficulty=00")
	}
	if !strings.HasPrefix(answer, "Wz") && !strings.HasPrefix(answer, "W1") {
		// base64 of a JSON array starting with "[" is "W" + something.
		t.Logf("answer prefix looks unusual: %.8s", answer)
	}
}

func TestFallbackPoWAnswer(t *testing.T) {
	answer := fallbackPoWAnswer("abc")
	if !strings.HasPrefix(answer, powErrorPrefix) {
		t.Fatalf("fallback answer must start with %q, got %q", powErrorPrefix, answer)
	}
}
