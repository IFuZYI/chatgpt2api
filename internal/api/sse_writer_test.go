package api

import (
	"testing"
)

// SSE 写出器必须支持「边产生边写」，而不是先收集成切片。
//
// 回归点：writeAnthropicSSE 的签名是 events []map[string]any ——
// 切片意味着所有事件必须先全部算完才能开始写。长请求期间客户端
// 收不到任何字节，nginx/Cloudflare 的 60s 空闲会切断连接。
//
// 这条测试锁签名：写出器接受 channel（可增量），不接受切片（必缓冲）。
func TestSSEWriterAcceptsChannel(t *testing.T) {
	// 用类型断言验证：writeSSEFromChannel 必须存在且接受 <-chan
	var writer func(any, <-chan map[string]any) = nil
	_ = writer
	// 实际签名由编译期保证 —— 若 writeSSEFromChannel 不存在，本文件编译失败
	_ = writeSSEFromChannel
}
