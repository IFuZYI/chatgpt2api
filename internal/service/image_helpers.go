package service

import (
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // 注册 webp 解码器，否则 ReadImage 会失败
)

// imageDimensions 读取图片宽高。解码失败返回 ok=false（与 Python 的 try/except 一致）。
func imageDimensions(path string) (int, int, bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer file.Close()
	config, _, err := image.DecodeConfig(file)
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return 0, 0, false
	}
	return config.Width, config.Height, true
}

// writeThumbnail 生成等比缩略图并写成 PNG（与 Python 的
// `image.thumbnail((320,320), LANCZOS)` + `save(format="PNG", optimize=True)` 等价）。
func writeThumbnail(source, target string, maxWidth, maxHeight int) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()

	decoded, _, err := image.Decode(file)
	if err != nil {
		return err
	}
	width, height := decoded.Bounds().Dx(), decoded.Bounds().Dy()
	if width <= 0 || height <= 0 {
		return errImageNotFound
	}

	scaleW := float64(maxWidth) / float64(width)
	scaleH := float64(maxHeight) / float64(height)
	scale := scaleW
	if scaleH < scale {
		scale = scaleH
	}
	if scale > 1 {
		scale = 1
	}
	newWidth := int(float64(width) * scale)
	newHeight := int(float64(height) * scale)
	if newWidth < 1 {
		newWidth = 1
	}
	if newHeight < 1 {
		newHeight = 1
	}

	// 用 CatmullRom 逼近 LANCZOS 的锐度。
	resized := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))
	draw.CatmullRom.Scale(resized, resized.Bounds(), decoded, decoded.Bounds(), draw.Over, nil)

	out, err := os.Create(target)
	if err != nil {
		return err
	}
	defer out.Close()
	return png.Encode(out, resized)
}

// diskUsage 返回文件系统容量（Go 标准库没有，用 syscall.Statfs 实现）。
func diskUsage(path string) (usage struct{ total, used, free uint64 }, err error) {
	return statfs(path)
}

// sniffImageMime 按魔数判断类型，用于没有扩展名的场景。
func sniffImageMime(payload []byte) string {
	if len(payload) < 12 {
		return "application/octet-stream"
	}
	switch {
	case payload[0] == 0xFF && payload[1] == 0xD8:
		return "image/jpeg"
	case string(payload[1:4]) == "PNG":
		return "image/png"
	case string(payload[0:4]) == "RIFF" && string(payload[8:12]) == "WEBP":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

var _ = jpeg.DefaultQuality
var _ = filepath.Ext
