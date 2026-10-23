package protocol

import (
	"io"
	"strings"
)

// stringReadCloser 把字符串包装成 io.ReadCloser（测试用）。
type stringReadCloser struct {
	reader *strings.Reader
}

func newStringReadCloser(text string) io.ReadCloser {
	return &stringReadCloser{reader: strings.NewReader(text)}
}

func (s *stringReadCloser) Read(p []byte) (int, error) { return s.reader.Read(p) }
func (s *stringReadCloser) Close() error               { return nil }

// readCloser 是 io.ReadCloser 的别名，让 fakeUpstream 的签名更短。
type readCloser = io.ReadCloser
