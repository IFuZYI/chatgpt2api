package protocol

import "bytes"

// bytesReader 隔离 bytes.NewReader，保持 helpers.go 的 import 干净。
func bytesReader(payload []byte) *bytes.Reader { return bytes.NewReader(payload) }
