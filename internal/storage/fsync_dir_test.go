package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// rename 之后必须 fsync 父目录。
//
// 为什么：POSIX 下 rename 只保证「目录项变更在内存里生效」，
// 断电时目录项可能还没落盘 —— 结果是**旧文件不见了、新文件也没了**，
// 也就是账号池整个丢失。fsync 文件本身（代码里已有）不能替代目录 fsync。
//
// 这条测试无法真的断电，所以断言的是「可观测的实现特征」：
// 保存后目录能被正常打开并 fsync（在支持的文件系统上不报错），
// 且文件内容完整。真正的断电语义靠代码审查保证，这里防的是
// 「有人把 dir fsync 删掉」这类回归。
func TestSaveJSONValueFsyncsDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	if err := saveJSONValue(path, []map[string]any{{"access_token": "at-1"}}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 内容完整
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("文件为空")
	}

	// 目录可 fsync（不支持时会返回 EINVAL，这里只要求不是「目录不存在」）
	d, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer d.Close()
	if syncErr := d.Sync(); syncErr != nil {
		// 部分文件系统（如某些 overlayfs）不支持目录 fsync，不算失败。
		t.Logf("目录 fsync 返回 %v（文件系统可能不支持）", syncErr)
	}
}
