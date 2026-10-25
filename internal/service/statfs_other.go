//go:build !linux

package service

import "os"

// statfs 是 linux 之外平台的实现。
//
// 为什么不用 syscall.Statfs：三个平台三套 API ——
// linux 是 `Statfs_t{Bsize, Blocks, Bfree, Bavail}`，
// darwin/BSD 是 `Statfs_t{Bsize uint32, Blocks, Bfree, Bavail}`，
// windows 只有 `syscall.Stat_t` 且语义不同，`syscall.Statfs` 根本不存在。
// 硬要在 `!linux` 里写一份会顾此失彼（实测：先写 darwin 版本，windows 直接
// `undefined: syscall.Stat_t` 编译失败）。
//
// 所以这里只做可移植的最小实现：用 os.Stat 确认路径可访问，容量字段留零。
//
// 影响面：diskUsage 的容量值只用于「按目标清理磁盘」与设置页用量显示。
// 非 Linux 平台上这两个功能看到 0 容量（清理目标无法达成），其余功能正常。
// 容量值不是核心链路 —— 比起让 darwin / windows 整体构建失败，这个降级更划算。
func statfs(path string) (usage struct{ total, used, free uint64 }, err error) {
	if _, err = os.Stat(path); err != nil {
		return usage, err
	}
	return usage, nil
}
