package protocol

import (
	"fmt"
	"os"
)

// debugf 输出诊断日志（CHATGPT2API_DEBUG=1 时启用）。
//
// 刻意不用 log 包：图片链路的问题往往需要和上游 SSE 对照，
// 混进框架日志反而难找。默认静默，避免污染生产输出。
func debugf(format string, args ...any) {
	if os.Getenv("CHATGPT2API_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
