package protocol

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"strconv"
	"strings"

	"golang.org/x/image/draw"
)

// mask 的两种「取通道」模式，对齐 PIL 的 Image.mode 语义。
//
// Python 的 _composite_mask 是三路分支（RGBA / L / 其他），但 L 那一支
// 是恒等操作（mask_img 本身就是 L，`alpha = mask_img` 等价于
// `mask_img.convert("L")`），所以合并成两态：取 alpha，或取灰度。
const (
	maskModeRGBA = "RGBA"
	maskModeGray = "gray"
)

const pngSignature = "\x89PNG\r\n\x1a\n"

// maxImagePixels 是单张图片允许的最大像素数，对齐 Pillow 的**硬错误**阈值。
//
// 数值来源（Pillow 13.0.0 的 Image._decompression_bomb_check）：
//   - MAX_IMAGE_PIXELS = 89_478_485，超过只发 DecompressionBombWarning（放行）
//   - pixels > 2 * MAX_IMAGE_PIXELS 才抛 DecompressionBombError（拒绝）
//
// 所以真正「Python 会拒」的边界是 2 × 89_478_485 = 178_956_970。
// 这里必须用硬错误阈值而不是 warning 阈值：用 warning 阈值会让 89M~179M
// 区间的图在 Go 侧被 502，而 Python 正常放行 —— 偏离权威实现。
//
// 为什么必须有这个上限：Go 的 image.Decode 会按 IHDR 声明的尺寸**真实分配**
// 像素缓冲。一个一百多字节的 PNG 只要把 IHDR 写成 40000x40000，解码就会尝试
// 分配约 6.4GB 并触发 `fatal error: runtime: out of memory` —— 这是运行时
// 致命错误，gin.Recovery() 抓不到，整个服务直接退出（实测去掉这条检查后
// 测试进程即刻崩溃）。
//
// 基线代码从不对用户字节做完整解码（只用 DecodeConfig 读头），mask 合成是
// 第一个完整解码用户图片的路径，所以这个上限必须加在这里。
// maxImagePixels 是单张图片允许的最大像素数。
//
// 原值 178_956_970 逐字对齐 Pillow 的硬错误阈值（2 × MAX_IMAGE_PIXELS），
// 但那个阈值是**库的**阈值，不是**本进程能承受的**预算：
// 178_956_970 像素 × 4 字节（NRGBA）= 683 MB，而 compositeOneMask 同时持有
// decoded（原始图）+ composited（NRGBA）+ alpha（Gray）+ PNG 编码缓冲，
// 峰值远超 683 MB。容器内存限制通常 512 MB–2 GB，单张图就能打穿。
//
// 这里改成由进程预算反推：默认 2000 万像素。
// 取这个值的理由：4096×4096 = 16,777,216 是常见的大图上限，必须放行；
// 2000 万给它留了余量，同时把峰值内存压在 ~80 MB NRGBA（4 字节/像素），
// 而不是原来的 683 MB。仍比 Pillow 的硬错误阈值小约 9 倍。
//
// 可用环境变量 CHATGPT2API_MAX_IMAGE_PIXELS 覆盖，便于大内存部署放宽。
const maxImagePixelsDefault = 20_000_000

// maxImagePixels 在 init 时由环境变量决定。
var maxImagePixels = resolveMaxImagePixels()

func resolveMaxImagePixels() int64 {
	if raw := strings.TrimSpace(os.Getenv("CHATGPT2API_MAX_IMAGE_PIXELS")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			return parsed
		}
	}
	return maxImagePixelsDefault
}

// maskScaler 是 alpha 缩放用的插值器。
//
// 抽成变量只为让测试能替换成计数器，验证「尺寸相同时不做缩放」。
// 生产代码从不写它；测试串行执行（本包无 t.Parallel），不引入竞争。
var maskScaler draw.Scaler = draw.CatmullRom

// checkImageDimensions 只读头部判定尺寸是否可接受，不解码像素。
//
// 单独抽出来是为了让「阈值边界」能被直接测到：边界测试只需要构造一个声明
// 大尺寸的头部，不必真的分配 width*height 的内存（14000x14000 是 784MB）。
func checkImageDimensions(payload []byte, label string) error {
	config, _, err := image.DecodeConfig(bytesReader(payload))
	if err != nil {
		return fmt.Errorf("cannot identify %s file: %w", label, err)
	}
	if config.Width <= 0 || config.Height <= 0 {
		return fmt.Errorf("cannot identify %s file: invalid dimensions %dx%d",
			label, config.Width, config.Height)
	}
	// 用 int64 相乘：32 位平台上 int 会溢出，让超大图绕过检查。
	if pixels := int64(config.Width) * int64(config.Height); pixels > maxImagePixels {
		return fmt.Errorf("%s dimensions %dx%d (%d pixels) exceed the %d pixel limit",
			label, config.Width, config.Height, pixels, maxImagePixels)
	}
	return nil
}

// decodeImageWithLimit 在完整解码之前先用 DecodeConfig 挡住超大图。
//
// 顺序很重要：DecodeConfig 只读头部，不分配像素缓冲；image.Decode 才分配。
// 先查尺寸再解码，才能在 OOM 发生之前返回 error。
func decodeImageWithLimit(payload []byte, label string) (image.Image, error) {
	if err := checkImageDimensions(payload, label); err != nil {
		return nil, err
	}
	decoded, _, err := image.Decode(bytesReader(payload))
	if err != nil {
		return nil, fmt.Errorf("cannot identify %s file: %w", label, err)
	}
	return decoded, nil
}

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
	decoded, err := decodeImageWithLimit(imagePayload, "image")
	if err != nil {
		return [3]string{}, err
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
	mask, err := decodeImageWithLimit(payload, "mask image")
	if err != nil {
		return nil, err
	}

	source := maskAlphaSource(mask, maskMode(payload, mask))

	// 关键：resize 到原图尺寸。Python 用 Image.LANCZOS，这里用 CatmullRom。
	//
	// 尺寸已经相同就跳过缩放：CatmullRom 在 1:1 时是恒等映射（实测
	// 0..1024 的随机灰度图 0 差异），但 2048x2048 仍要花约 43% 的总耗时。
	// OpenAI 规范要求 mask 与原图同尺寸，所以绝大多数请求走这条捷径。
	if source.Bounds().Dx() == width && source.Bounds().Dy() == height {
		return source, nil
	}

	alpha := image.NewGray(image.Rect(0, 0, width, height))
	maskScaler.Scale(alpha, alpha.Bounds(), source, source.Bounds(), draw.Src, nil)
	return alpha, nil
}

// maskAlphaSource 复刻 Python 的取通道分支：
//
//	if mask_img.mode == "RGBA": alpha = mask_img.split()[3]
//	else:                       alpha = mask_img.convert("L")
//
// （Python 的 `elif mode == "L": alpha = mask_img` 与 else 分支等价：
// 对 L 图调 convert("L") 是恒等操作。）
func maskAlphaSource(mask image.Image, mode string) *image.Gray {
	bounds := mask.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	gray := image.NewGray(image.Rect(0, 0, width, height))

	// 16 位灰度（PIL 模式 I;16）→ convert("L") 是把 >255 的值截到 255：
	// 实测 0x1234 / 0x0100 / 0x00FF 全部得到 255。Go 的 GrayModel 会按
	// 16→8 位缩放（0x1234 → 18），语义相反（0=编辑，255=保留），所以单独对齐。
	//
	// 不需要判断 mode：16 位灰度 PNG（color type 0）恒为 maskModeGray，
	// 而 16 位灰度+alpha（color type 4）会被 pngMaskMode 判成 RGBA、
	// 由 Go 解成 NRGBA64，走不到这里。
	if typed, ok := mask.(*image.Gray16); ok {
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

// maskMode 复刻 PIL 打开图片后的 Image.mode，只保留影响取通道的两类。
func maskMode(payload []byte, decoded image.Image) string {
	if mode, ok := pngMaskMode(payload); ok {
		return mode
	}
	if mode, ok := webpMaskMode(payload); ok {
		return mode
	}
	// 其余格式（JPEG / GIF…）：PIL 里带真实 alpha 的模式只有 RGBA，
	// Go 的 webp 解码器把带 alpha 的图解成 NRGBA/NYCbCrA，正好对应。
	// GIF 解成 Paletted、JPEG 解成 YCbCr，都归到「转灰度」。
	switch decoded.(type) {
	case *image.NRGBA, *image.NRGBA64, *image.NYCbCrA:
		return maskModeRGBA
	default:
		return maskModeGray
	}
}

// webpMaskMode 判定 WebP 在 PIL 里的模式。
//
// 不能只看 Go 的解码类型：无损 WebP（VP8L）无论码流带不带 alpha，Go 都返回
// *image.NRGBA，而 PIL 只在码流真的带 alpha 时才报 RGBA —— 不带 alpha 时报
// RGB，走 convert("L")。把无 alpha 的 VP8L 当 RGBA 去取 alpha，会得到 255
// （解码器把缺失的 alpha 填满），等于把整张 mask 读成「全部保留」。
//
// 判定依据是容器里的 alpha 标志位：
//   - VP8X（扩展格式）：byte 20 的 bit 4（0x10）是 Alpha 标志
//   - VP8L（无损）：5 字节头里第 21..24 字节组成的 32 位小端值的 bit 28
//   - VP8（有损，无扩展头）：格式本身不支持 alpha，恒为 RGB
func webpMaskMode(payload []byte) (string, bool) {
	if len(payload) < 21 || !bytes.HasPrefix(payload, []byte("RIFF")) ||
		!bytes.Equal(payload[8:12], []byte("WEBP")) {
		return "", false
	}
	switch string(payload[12:16]) {
	case "VP8X":
		// 扩展格式：flags 在 byte 20，Alpha 是 bit 4。
		if len(payload) < 21 {
			return "", false
		}
		if payload[20]&0x10 != 0 {
			return maskModeRGBA, true
		}
		return maskModeGray, true
	case "VP8L":
		// 无损：5 字节头在 20..24，签名 0x2F + 14 位宽 + 14 位高 + 1 位 alpha + 3 位版本。
		if len(payload) < 25 || payload[20] != 0x2f {
			return "", false
		}
		bits := uint32(payload[21]) | uint32(payload[22])<<8 |
			uint32(payload[23])<<16 | uint32(payload[24])<<24
		if (bits>>28)&1 != 0 {
			return maskModeRGBA, true
		}
		return maskModeGray, true
	case "VP8 ":
		// 有损且没有 VP8X 扩展头：格式本身不携带 alpha。
		return maskModeGray, true
	}
	return "", false
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
		return maskModeGray, true
	default: // 0 = 灰度（含 16 位 I;16）、2 = 真彩、3 = 调色板 → convert("L")
		return maskModeGray, true
	}
}
