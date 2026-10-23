package upstream

import (
	"encoding/base64"
	"encoding/json"
	"math/rand"
	"strings"
	"time"
)

// turnstile 解释器。逐分支复刻 utils/turnstile.py 的 solve_turnstile_token。
//
// 上游把一段「字节码」base64 + XOR 后下发，里面是一串 [opcode, args...] 元组。
// 每个 opcode 对应一个闭包，操作一个共享的 process_map。最终 func_3 把结果
// base64 后写进 result —— 这就是 turnstile token。
//
// 这不是可以「简化」的代码：opcode 的语义、特殊字符串映射表、甚至
// func_17 里 window.performance.now 的纳秒算法都必须逐字一致。

// orderedMap 复刻 Python 的 OrderedMap（func_7 里 window.Reflect.set 用）。
type orderedMap struct {
	keys   []string
	values map[string]any
}

func newOrderedMap() *orderedMap {
	return &orderedMap{values: map[string]any{}}
}

func (m *orderedMap) add(key string, value any) {
	if _, exists := m.values[key]; !exists {
		m.keys = append(m.keys, key)
	}
	m.values[key] = value
}

// turnstileToString 复刻 _turnstile_to_str。
func turnstileToString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "undefined"
	case float64:
		return formatFloat(typed)
	case string:
		special := map[string]string{
			"window.Math":            "[object Math]",
			"window.Reflect":         "[object Reflect]",
			"window.performance":     "[object Performance]",
			"window.localStorage":    "[object Storage]",
			"window.Object":          "function Object() { [native code] }",
			"window.Reflect.set":     "function set() { [native code] }",
			"window.performance.now": "function () { [native code] }",
			"window.Object.create":   "function create() { [native code] }",
			"window.Object.keys":     "function keys() { [native code] }",
			"window.Math.random":     "function random() { [native code] }",
		}
		if mapped, ok := special[typed]; ok {
			return mapped
		}
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		allStrings := true
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				allStrings = false
				break
			}
			parts = append(parts, text)
		}
		if allStrings {
			return strings.Join(parts, ",")
		}
		return formatValue(typed)
	case *orderedMap:
		return formatValue(typed)
	default:
		return formatValue(typed)
	}
}

// formatFloat 复刻 Python 的 str(float)：整数也带 ".0"。
func formatFloat(value float64) string {
	if value == float64(int64(value)) {
		return strconvFormatInt(int64(value)) + ".0"
	}
	return strconvFormatFloat(value)
}

func formatValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// xorString 复刻 _xor_string：逐字符 XOR，key 循环使用。
func xorString(text, key string) string {
	if key == "" {
		return text
	}
	var builder strings.Builder
	builder.Grow(len(text))
	for index, ch := range []rune(text) {
		builder.WriteRune(ch ^ rune(key[index%len(key)]))
	}
	return builder.String()
}

// turnstileState 是 process_map 的 Go 版：用 map[int]any + 闭包表。
type turnstileState struct {
	values  map[int]any
	result  string
	started time.Time
	rng     *rand.Rand
}

// SolveTurnstileToken 复刻 solve_turnstile_token。失败返回空字符串。
func SolveTurnstileToken(dx, p string) string {
	decoded, err := base64.StdEncoding.DecodeString(dx)
	if err != nil {
		return ""
	}
	plain := xorString(string(decoded), p)

	var tokenList [][]any
	if err := json.Unmarshal([]byte(plain), &tokenList); err != nil {
		return ""
	}

	state := &turnstileState{
		values:  map[int]any{},
		started: time.Now(),
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	// opcode 表。键是 token[0] 的值（JSON 解出来是 float64）。
	state.values[9] = tokenList
	state.values[10] = "window"
	state.values[16] = p

	ops := map[int]func(args []any){
		1: func(args []any) { // process_map[e] = xor(process_map[e], process_map[t])
			e, ok1 := intArg(args, 0)
			t, ok2 := intArg(args, 1)
			if !ok1 || !ok2 {
				return
			}
			state.values[e] = xorString(turnstileToString(state.values[e]), turnstileToString(state.values[t]))
		},
		2: func(args []any) { // process_map[e] = t
			e, ok1 := intArg(args, 0)
			if !ok1 || len(args) < 2 {
				return
			}
			state.values[e] = args[1]
		},
		3: func(args []any) { // result = b64(e)
			if len(args) < 1 {
				return
			}
			state.result = base64.StdEncoding.EncodeToString([]byte(turnstileToString(args[0])))
		},
		5: func(args []any) { // 拼接
			e, ok1 := intArg(args, 0)
			t, ok2 := intArg(args, 1)
			if !ok1 || !ok2 {
				return
			}
			current := state.values[e]
			incoming := state.values[t]
			switch typed := current.(type) {
			case []any:
				state.values[e] = append(append([]any{}, typed...), incoming)
				return
			}
			state.values[e] = turnstileToString(current) + turnstileToString(incoming)
		},
		6: func(args []any) { // 点号拼接 + 特判 window.document.location
			e, ok1 := intArg(args, 0)
			t, ok2 := intArg(args, 1)
			n, ok3 := intArg(args, 2)
			if !ok1 || !ok2 || !ok3 {
				return
			}
			tv, tvOK := state.values[t].(string)
			nv, nvOK := state.values[n].(string)
			if !tvOK || !nvOK {
				return
			}
			value := tv + "." + nv
			if value == "window.document.location" {
				value = "https://chatgpt.com/"
			}
			state.values[e] = value
		},
		7: func(args []any) { // func_7：Reflect.set 或调用函数
			e, ok := intArg(args, 0)
			if !ok {
				return
			}
			target := state.values[e]
			values := make([]any, 0, len(args)-1)
			for _, arg := range args[1:] {
				index, ok := intArgValue(arg)
				if !ok {
					continue
				}
				values = append(values, state.values[index])
			}
			if text, ok := target.(string); ok && text == "window.Reflect.set" {
				if len(values) < 3 {
					return
				}
				obj, ok := values[0].(*orderedMap)
				if !ok {
					return
				}
				key, keyOK := values[1].(string)
				if !keyOK {
					return
				}
				obj.add(key, values[2])
				return
			}
			if fn, ok := target.(func([]any)); ok {
				fn(values)
			}
		},
		8: func(args []any) {
			e, ok1 := intArg(args, 0)
			t, ok2 := intArg(args, 1)
			if !ok1 || !ok2 {
				return
			}
			state.values[e] = state.values[t]
		},
		14: func(args []any) { // json.loads
			e, ok1 := intArg(args, 0)
			t, ok2 := intArg(args, 1)
			if !ok1 || !ok2 {
				return
			}
			var decoded any
			if err := json.Unmarshal([]byte(turnstileToString(state.values[t])), &decoded); err != nil {
				return
			}
			state.values[e] = decoded
		},
		15: func(args []any) { // json.dumps
			e, ok1 := intArg(args, 0)
			t, ok2 := intArg(args, 1)
			if !ok1 || !ok2 {
				return
			}
			encoded, err := json.Marshal(state.values[t])
			if err != nil {
				return
			}
			state.values[e] = string(encoded)
		},
		17: func(args []any) { // window.performance.now / Object.create / keys / Math.random
			e, ok1 := intArg(args, 0)
			t, ok2 := intArg(args, 1)
			if !ok1 || !ok2 {
				return
			}
			callArgs := make([]any, 0, len(args)-2)
			for _, arg := range args[2:] {
				index, ok := intArgValue(arg)
				if !ok {
					continue
				}
				callArgs = append(callArgs, state.values[index])
			}
			switch target := state.values[t].(type) {
			case string:
				switch target {
				case "window.performance.now":
					elapsedNS := time.Since(state.started).Nanoseconds()
					state.values[e] = (float64(elapsedNS) + state.rng.Float64()) / 1e6
				case "window.Object.create":
					state.values[e] = newOrderedMap()
				case "window.Object.keys":
					if len(callArgs) > 0 {
						if text, ok := callArgs[0].(string); ok && text == "window.localStorage" {
							state.values[e] = []any{
								"STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4",
								"STATSIG_LOCAL_STORAGE_STABLE_ID",
								"client-correlated-secret",
								"oai/apps/capExpiresAt",
								"oai-did",
								"STATSIG_LOCAL_STORAGE_LOGGING_REQUEST",
								"UiState.isNavigationCollapsed.1",
							}
						}
					}
				case "window.Math.random":
					state.values[e] = state.rng.Float64()
				}
			case func([]any):
				state.values[e] = nil
				target(callArgs)
			}
		},
		18: func(args []any) { // b64decode
			e, ok := intArg(args, 0)
			if !ok {
				return
			}
			decoded, err := base64.StdEncoding.DecodeString(turnstileToString(state.values[e]))
			if err != nil {
				return
			}
			state.values[e] = string(decoded)
		},
		19: func(args []any) { // b64encode
			e, ok := intArg(args, 0)
			if !ok {
				return
			}
			state.values[e] = base64.StdEncoding.EncodeToString([]byte(turnstileToString(state.values[e])))
		},
		20: func(args []any) { // 相等则调用
			e, ok1 := intArg(args, 0)
			t, ok2 := intArg(args, 1)
			n, ok3 := intArg(args, 2)
			if !ok1 || !ok2 || !ok3 {
				return
			}
			if !valuesEqual(state.values[e], state.values[t]) {
				return
			}
			fn, ok := state.values[n].(func([]any))
			if !ok {
				return
			}
			callArgs := make([]any, 0, len(args)-3)
			for _, arg := range args[3:] {
				index, ok := intArgValue(arg)
				if !ok {
					continue
				}
				callArgs = append(callArgs, state.values[index])
			}
			fn(callArgs)
		},
		21: func(args []any) {},
		23: func(args []any) { // 非 nil 且可调用
			e, ok1 := intArg(args, 0)
			t, ok2 := intArg(args, 1)
			if !ok1 || !ok2 || state.values[e] == nil {
				return
			}
			fn, ok := state.values[t].(func([]any))
			if !ok {
				return
			}
			callArgs := make([]any, 0, len(args)-2)
			for _, arg := range args[2:] {
				index, ok := intArgValue(arg)
				if !ok {
					continue
				}
				callArgs = append(callArgs, state.values[index])
			}
			fn(callArgs)
		},
		24: func(args []any) { // 点号拼接
			e, ok1 := intArg(args, 0)
			t, ok2 := intArg(args, 1)
			n, ok3 := intArg(args, 2)
			if !ok1 || !ok2 || !ok3 {
				return
			}
			tv, tvOK := state.values[t].(string)
			nv, nvOK := state.values[n].(string)
			if !tvOK || !nvOK {
				return
			}
			state.values[e] = tv + "." + nv
		},
	}
	for opcode, fn := range ops {
		state.values[opcode] = fn
	}

	for _, token := range tokenList {
		if len(token) == 0 {
			continue
		}
		opcode, ok := intArgValue(token[0])
		if !ok {
			continue
		}
		fn, ok := state.values[opcode].(func([]any))
		if !ok {
			continue
		}
		func() {
			defer func() { _ = recover() }()
			fn(token[1:])
		}()
	}

	return state.result
}

func intArg(args []any, index int) (int, bool) {
	if index >= len(args) {
		return 0, false
	}
	return intArgValue(args[index])
}

func intArgValue(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	default:
		return 0, false
	}
}

func valuesEqual(left, right any) bool {
	return turnstileToString(left) == turnstileToString(right)
}
