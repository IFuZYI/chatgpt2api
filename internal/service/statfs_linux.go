//go:build linux

package service

import "syscall"

// statfs 复刻 Python 的 shutil.disk_usage。
func statfs(path string) (usage struct{ total, used, free uint64 }, err error) {
	var stat syscall.Statfs_t
	if err = syscall.Statfs(path, &stat); err != nil {
		return usage, err
	}
	blockSize := uint64(stat.Bsize)
	usage.total = stat.Blocks * blockSize
	usage.free = stat.Bavail * blockSize
	usage.used = usage.total - stat.Bfree*blockSize
	return usage, nil
}
