package protocol

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"

	"golang.org/x/image/draw"
)

// mask 的三种「取通道」模式，对齐 PIL 的 Image.mode 语义。
//
// Python 的 _composite_mask 只对 RGBA 与 L 走特殊分支，其余模式一律
// convert("L")，所以这里只需要三态，不需要完整的 PIL 模式集合。
const (
	maskModeRGBA  = "RGBA"
	maskModeL     = "L"
	maskModeOther = "other"
)

const pngSignature = "\x89PNG\r\n\x1a\n"

// CompositeMask 复刻 services/protocol/openai_v1_image_edit.py:20-49 的
// _composite_mask：把 mask 的 alpha 通道合成到图片中，标识需要编辑的区域。
//
// mask 的透明区域（低 alpha）= 需要编辑的区域，
// mask 的不透明区域（高 alpha）= 保留的区域。
// 如果无 mask 则返回原图（Python 也是 `return images`，不复制切片）。
//
// 语义必须与 Python 逐条对齐：
//   - mask 索引回退：i < len(masks) 用 masks[i]，否则用 masks[len-1]
//   - 取通道：RGBA 取 alpha；L 直接用；其他转灰度
//   - alpha 缩放到原图尺寸（Python 用 LANCZOS，这里用 CatmullRom，最近的等价）
//   - 输出统一 PNG，mime 固定 image/png，filename 保留原图名
func CompositeMask(images, masks [][3]string) ([][3]string, error) {
	if len(masks) == 0 {
		return images, nil
	}
	result := make([][3]string, 0, len(images))
	for index, imageTuple := range images {
		// mask 索引：有第 i 个用第 i 个，否则回退到最后一个。
		// Python: masks[i][0] if i < len(masks) else masks[-1][0]
		maskIndex := index
		if index >= len(masks) {
			maskIndex = len(masks) - 1
		}
		composited, err := compositeOneMask(imageTuple, masks[maskIndex])
		if err != nil {
			return nil, err
		}
		result = append(result, composited)
	}
	return result, nil
}

// compositeOneMask 处理单张图：解码 → 取 mask 的 alpha → 缩放 → 写回 → 编码 PNG。
//
// 错误语义对齐 Python：PIL 打不开图会抛异常，冒泡到 LoggedCall 变成 502。
// 这里同样返回 error 让上层决定状态码。
func compositeOneMask(imageTuple, maskTuple [3]string) ([3]string, error) {
	imagePayload, err := decodeBase64String(imageTuple[0])
	if err != nil {
		return [3]string{}, fmt.Errorf("cannot identify image file: %w", err)
	}
	decoded, _, err := image.Decode(bytesReader(imagePayload))
	if err != nil {
		return [3]string{}, fmt.Errorf("cannot identify image file: %w", err)
	}

	bounds := decoded.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return [3]string{}, fmt.Errorf("cannot identify image file: empty image")
	}

	alpha, err := maskAlphaChannel(maskTuple[0], width, height)
	if err != nil {
		return [3]string{}, err
	}

	// 对应 Python 的 img.convert("RGBA") + img.putalpha(alpha)：
	// 先落到非预乘的 NRGBA（PIL 的 RGBA 也是直通 alpha），再逐像素写第 4 通道。
	composited := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Draw(composited, composited.Bounds(), decoded, bounds.Min, draw.Src)
	for y := 0; y < height; y++ {
		row := composited.Pix[y*composited.Stride:]
		alphaRow := alpha.Pix[y*alpha.Stride:]
		for x := 0; x < width; x++ {
			row[x*4+3] = alphaRow[x]
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, composited); err != nil {
		return [3]string{}, fmt.Errorf("encode composited image: %w", err)
	}

	// filename 保留原图名，mime 固定 image/png（Python 的第三个字段是常量）。
	return [3]string{base64.StdEncoding.EncodeToString(buf.Bytes()), imageTuple[1], "image/png"}, nil
}

// maskAlphaChannel 取出 mask 的 alpha 通道并缩放到原图尺寸。
func maskAlphaChannel(encodedMask string, width, height int) (*image.Gray, error) {
	payload, err := decodeBase64String(encodedMask)
	if err != nil {
		return nil, fmt.Errorf("cannot identify mask image file: %w", err)
	}
	mask, _, err := image.Decode(bytesReader(payload))
	if err != nil {
		return nil, fmt.Errorf("cannot identify mask image file: %w", err)
	}

	source := maskAlphaSource(mask, maskMode(payload, mask))

	// 关键：resize 到原图尺寸。Python 用 Image.LANCZOS，这里用 CatmullRom。
	// 1:1 时 CatmullRom 是恒等映射（实测 0/255 边界值不漂移），与 PIL 一致。
	alpha := image.NewGray(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(alpha, alpha.Bounds(), source, source.Bounds(), draw.Src, nil)
	return alpha, nil
}

// maskAlphaSource 复刻 Python 的取通道分支：
//
//	if mask_img.mode == "RGBA": alpha = mask_img.split()[3]
//	elif mask_img.mode == "L":  alpha = mask_img
//	else:                       alpha = mask_img.convert("L")
func maskAlphaSource(mask image.Image, mode string) *image.Gray {
	bounds := mask.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	gray := image.NewGray(image.Rect(0, 0, width, height))

	// 16 位灰度（PIL 模式 I;16）→ convert("L") 是把 >255 的值截到 255：
	// 实测 0x1234 / 0x0100 / 0x00FF 全部得到 255。Go 的 GrayModel 会按
	// 16→8 位缩放（0x1234 → 18），语义相反（0=编辑，255=保留），所以单独对齐。
	if typed, ok := mask.(*image.Gray16); ok && mode != maskModeRGBA {
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				value := typed.Gray16At(bounds.Min.X+x, bounds.Min.Y+y).Y
				if value > 255 {
					value = 255
				}
				gray.Pix[y*gray.Stride+x] = uint8(value)
			}
		}
		return gray
	}

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			value := mask.At(bounds.Min.X+x, bounds.Min.Y+y)
			if mode == maskModeRGBA {
				// split()[3]：只取 alpha，与 RGB 无关。
				_, _, _, alpha := value.RGBA()
				gray.Pix[y*gray.Stride+x] = uint8(alpha >> 8)
				continue
			}
			// convert("L")：PIL 与 Go 都用 ITU-R 601-2 亮度系数
			// (0.299, 0.587, 0.114)，调色板图也一样（红 76 / 蓝 29）。
			gray.Pix[y*gray.Stride+x] = luminanceOf(value)
		}
	}
	return gray
}

// luminanceOf 复刻 PIL 的 convert("L")：L = R*19595 + G*38470 + B*7471（16 位定点）。
//
// 必须取**非预乘**的 RGB：PIL 的 L 只丢 alpha、不改 RGB，而 Go 的
// color.GrayModel 走的是 c.RGBA()（预乘）。灰度+alpha 的图（LA）解码成
// NRGBA{100,100,100,0} 时预乘后是 (0,0,0,0)，会得到 0 而不是 100 ——
// 而 0/255 在 mask 语义里正好是「编辑/保留」的两端，取错等于把区域反过来。
func luminanceOf(value color.Color) uint8 {
	nonPremultiplied := color.NRGBAModel.Convert(value).(color.NRGBA)
	luminance := (uint32(nonPremultiplied.R)*19595 +
		uint32(nonPremultiplied.G)*38470 +
		uint32(nonPremultiplied.B)*7471 + 1<<15) >> 16
	return uint8(luminance)
}

// maskMode 复刻 PIL 打开图片后的 Image.mode，只保留影响取通道的三类。
func maskMode(payload []byte, decoded image.Image) string {
	if mode, ok := pngMaskMode(payload); ok {
		return mode
	}
	// 非 PNG（JPEG / WebP / GIF…）：PIL 里带真实 alpha 的模式只有 RGBA，
	// Go 的 webp 解码器把带 alpha 的图解成 NRGBA，正好对应。
	// GIF 解成 Paletted、JPEG 解成 YCbCr，都归到「转灰度」。
	switch decoded.(type) {
	case *image.NRGBA, *image.NRGBA64:
		return maskModeRGBA
	default:
		return maskModeOther
	}
}

// pngMaskMode 从 PNG 的 IHDR 读位深与颜色类型，按 PIL 的 _MODES 映射表判定模式。
//
// 为什么要读 IHDR 而不是看 Go 的解码类型：PIL 把「灰度+alpha」的 8 位 PNG 归为
// LA（convert("L") 只取灰度、忽略 alpha），而 Go 解成 NRGBA —— 只看类型会把
// LA 当成 RGBA 去取 alpha，取错通道。
func pngMaskMode(payload []byte) (string, bool) {
	if len(payload) < 26 || !bytes.HasPrefix(payload, []byte(pngSignature)) {
		return "", false
	}
	// IHDR 是第一个 chunk：8 字节签名 + 4 字节长度 + 4 字节 "IHDR"，
	// 之后 width(4) height(4) bitDepth(1) colorType(1) → 位深在 24，颜色类型在 25。
	depth, colorType := payload[24], payload[25]
	switch colorType {
	case 6: // 真彩 + alpha → RGBA，取 alpha 通道
		return maskModeRGBA, true
	case 4: // 灰度 + alpha：8 位是 LA（转灰度），16 位 PIL 归到 RGBA（取 alpha）
		if depth == 16 {
			return maskModeRGBA, true
		}
		return maskModeOther, true
	case 0: // 灰度：1/2/4/8 位在 PIL 里值域都是 0..255（L 直接用），16 位是 I;16
		if depth == 16 {
			return maskModeOther, true
		}
		return maskModeL, true
	default: // 2 = 真彩、3 = 调色板 → convert("L")
		return maskModeOther, true
	}
}
