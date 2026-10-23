package upstream

import (
	"crypto/rand"
	"encoding/hex"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

// NewHTTPClient 是 NewClient 的底层构造函数，单独导出便于测试与复用。
func NewHTTPClient(opts ClientOptions) (tls_client.HttpClient, BrowserProfile, error) {
	return newClientImpl(opts)
}

// newUUID 复刻 Python 的 uuid4() 字符串形式（带连字符）。
func newUUID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 随机源不可用时退回全零（不会发生，但避免 panic）。
		return "00000000-0000-4000-8000-000000000000"
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10
	encoded := hex.EncodeToString(buf[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" +
		encoded[16:20] + "-" + encoded[20:32]
}

// headerNames 把 http.Header 的键按插入顺序展开（用于调试与测试断言）。
func headerNames(headers http.Header) []string {
	order, ok := headers[http.HeaderOrderKey]
	if ok {
		return order
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		if name == http.HeaderOrderKey || name == http.PHeaderOrderKey {
			continue
		}
		names = append(names, name)
	}
	sortStrings(names)
	return names
}
