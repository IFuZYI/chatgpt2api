package api

import "os"

// statFile 是 os.Stat 的薄包装。
func statFile(path string) (os.FileInfo, error) { return os.Stat(path) }
