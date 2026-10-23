package protocol

import "context"

// 测试用的 StreamImage 默认实现：把 sseLines 转成 channel。
// 各 fake 类型可以内嵌 streamStub 获得这个方法。
type streamStub struct {
	sseLines []string
}

func (s *streamStub) StreamImage(ctx context.Context, request ImageStreamRequest) (<-chan string, <-chan error, error) {
	payloads := make(chan string, len(s.sseLines))
	errs := make(chan error, 1)
	for _, line := range s.sseLines {
		payloads <- line
	}
	close(payloads)
	close(errs)
	return payloads, errs, nil
}
