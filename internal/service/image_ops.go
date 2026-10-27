package service

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CompressImages 复刻 compress_images：重编码 PNG，只有变小才替换。
//
// 注意 Python 的 `Image.save(format="PNG", optimize=True)` 本身不降画质，
// 只是重新打包；Go 的 png.Encode 用默认压缩级别，效果等价。
func (s *ImageService) CompressImages() map[string]any {
	root := s.cfg.ImagesDir()
	var saved int64
	count := 0

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(strings.ToLower(info.Name()), ".png") {
			return nil
		}
		original := info.Size()
		newSize, ok := reencodePNG(path)
		if !ok || newSize >= original {
			return nil
		}
		saved += original - newSize
		count++
		return nil
	})

	return map[string]any{
		"compressed":  count,
		"saved_bytes": saved,
		"saved_mb":    int(saved / (1024 * 1024)),
	}
}

func reencodePNG(path string) (int64, bool) {
	source, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	// defer 而不是显式 Close：png.Decode 对畸形 PNG 可能 panic
	// （解码器内部越界），显式 Close 会被跳过 —— 批量压缩几百张图时
	// 累积的 fd 泄漏会让进程撞上 "too many open files"。
	defer source.Close()
	decoded, err := png.Decode(source)
	if err != nil {
		return 0, false
	}

	var buffer bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestCompression}
	if err := encoder.Encode(&buffer, decoded); err != nil {
		return 0, false
	}

	// 压缩后反而变大时保留原文件。
	//
	// png.BestCompression 对已经压得很紧的图（或本身是 JPEG 转存的 PNG）
	// 可能产出更大的字节流。原实现无条件 rename 覆盖，等于「压缩」把文件弄大。
	originalSize := int64(0)
	if info, statErr := os.Stat(path); statErr == nil {
		originalSize = info.Size()
	}
	if originalSize > 0 && int64(buffer.Len()) >= originalSize {
		return originalSize, false
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buffer.Bytes(), 0o644); err != nil {
		return 0, false
	}
	size := int64(buffer.Len())
	// 保留原 mtime：否则「压缩」这个动作会把文件的修改时间刷新成现在，
	// 于是按 mtime 判断的保留期清理（image_retention_days）对这些文件
	// 永远不会触发 —— 一张 90 天前的老图压缩后变成「刚刚创建」。
	if info, statErr := os.Stat(path); statErr == nil {
		_ = os.Chtimes(tmp, info.ModTime(), info.ModTime())
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return 0, false
	}
	return size, true
}

// DeleteToTarget 复刻 delete_to_target：按 mtime 从旧到新删，直到剩余空间达标。
func (s *ImageService) DeleteToTarget(targetFreeMB int, dryRun bool) map[string]any {
	root := s.cfg.ImagesDir()
	usage, _ := diskUsage(root)
	currentFreeMB := int(usage.free / (1024 * 1024))
	if currentFreeMB >= targetFreeMB && !dryRun {
		return map[string]any{
			"removed":         0,
			"current_free_mb": currentFreeMB,
			"target_free_mb":  targetFreeMB,
			"done":            true,
		}
	}

	type candidate struct {
		path    string
		size    int64
		modTime int64
	}
	files := make([]candidate, 0)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(strings.ToLower(info.Name()), ".png") {
			return nil
		}
		files = append(files, candidate{path: path, size: info.Size(), modTime: info.ModTime().UnixNano()})
		return nil
	})
	sort.SliceStable(files, func(i, j int) bool { return files[i].modTime < files[j].modTime })

	var removed int
	var freed int64
	for _, file := range files {
		if currentFreeMB+int(freed/(1024*1024)) >= targetFreeMB {
			break
		}
		if !dryRun {
			if rel, err := filepath.Rel(root, file.path); err == nil {
				relSlash := filepath.ToSlash(rel)
				if thumb, thumbErr := s.ThumbnailPath(relSlash); thumbErr == nil {
					_ = os.Remove(thumb)
				}
				s.removeTags(relSlash)
			}
			// 只有真删成功才记账。
			//
			// 原来 `_ = os.Remove(...)` 丢错误却照样 freed += size / removed++，
			// 于是不可删的文件（chattr +i、EBUSY、只读挂载）会被报成
			// removed:1 而实际还在盘上 —— 调度器以为腾出了空间就停手，
			// 磁盘继续满着。
			if err := os.Remove(file.path); err != nil {
				continue
			}
		}
		freed += file.size
		removed++
	}

	if !dryRun {
		removeEmptyDirs(root)
		removeEmptyDirs(s.cfg.ImageThumbnailsDir())
	}

	finalFree := currentFreeMB + int(freed/(1024*1024))
	return map[string]any{
		"removed":         removed,
		"freed_mb":        int(freed / (1024 * 1024)),
		"target_free_mb":  targetFreeMB,
		"current_free_mb": finalFree,
		"done":            finalFree >= targetFreeMB,
		"dry_run":         dryRun,
	}
}

// DownloadZip 复刻修复后的 download_images_zip（ADR-0003 的 intentional-divergence）：
// 本地文件优先，缺失时回退 WebDAV 读取，重名追加 _2 后缀。
//
// Python 版里这个函数被定义了两次，生效的那份没有 WebDAV 回退分支，
// 导致 WebDAV 模式下 zip 下载实际不可用。这里按「修复后」语义实现 ——
// 回退规则与 get_bytes 共用 ReadImage（索引 webdav:true 才回远端）。
func (s *ImageService) DownloadZip(paths []string) ([]byte, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	added := 0
	usedNames := map[string]bool{}

	for _, item := range paths {
		safe, err := safeRelativePath(item)
		if err != nil {
			continue
		}
		payload, _, readErr := s.ReadImage(safe)
		if readErr != nil {
			continue
		}

		name := filepath.Base(safe)
		if usedNames[name] {
			stem := strings.TrimSuffix(name, filepath.Ext(name))
			suffix := filepath.Ext(name)
			counter := 2
			for usedNames[fmt.Sprintf("%s_%d%s", stem, counter, suffix)] {
				counter++
			}
			name = fmt.Sprintf("%s_%d%s", stem, counter, suffix)
		}
		usedNames[name] = true

		entry, err := writer.Create(name)
		if err != nil {
			continue
		}
		if _, err := entry.Write(payload); err != nil {
			continue
		}
		added++
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}
	if added == 0 {
		return nil, errImageNotFound
	}
	return buffer.Bytes(), nil
}
