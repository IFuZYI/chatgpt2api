package upstream

import (
	"bytes"
	"io"
	"os"
)

// newBytesReader 把字节切成 Reader。
func newBytesReader(payload []byte) io.Reader { return bytes.NewReader(payload) }

// readFileImpl 读文件，不存在时返回 (nil, nil)。
func readFileImpl(path string) ([]byte, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return payload, nil
}
