package service

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// localTimestamp 复刻 Python 的 datetime.now().strftime("%Y-%m-%d %H:%M:%S")：
// 本地时区、秒级精度。磁盘上已有数据全是这个格式，不能改成 RFC3339。
func localTimestamp() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

// timeAfterSecond 返回 1 秒定时通道，对齐 Python 的
// `self._image_slot_condition.wait(timeout=1.0)` 轮询粒度。
func timeAfterSecond() <-chan time.Time {
	return time.After(time.Second)
}

// anonymizeToken 复刻 utils/helper.py 的 anonymize_token：
// 取 sha256 前 10 位，前缀 "token:"。
func anonymizeToken(token any) string {
	value := stringValue(token)
	if value == "" {
		return "token:empty"
	}
	sum := sha256.Sum256([]byte(value))
	return "token:" + hex.EncodeToString(sum[:])[:10]
}
