package protocol

import "crypto/rand"
import "encoding/hex"

// newUUIDValue 生成带连字符的 uuid4 字符串（与 Python 的 uuid4() 输出形态一致）。
func newUUIDValue() string {
	encoded := newUUIDHex()
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" +
		encoded[16:20] + "-" + encoded[20:32]
}

// newUUIDHex 生成不带连字符的 uuid4 十六进制串（与 Python 的 uuid4().hex 同形）。
//
// Responses 协议的 id 都用它拼前缀（resp_ / msg_ / ig_ 等）：Python 侧是
// f"resp_{uuid.uuid4().hex}"，32 位无连字符，不是 newUUIDValue 的 36 位。
func newUUIDHex() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "00000000000040008000000000000000"
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return hex.EncodeToString(buf[:])
}
