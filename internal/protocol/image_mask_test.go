package protocol

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/draw"
)

// encodePNG 把任意 image.Image 编成 base64 PNG 三元组。
func encodePNG(t *testing.T, img image.Image, filename string) [3]string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return [3]string{base64.StdEncoding.EncodeToString(buf.Bytes()), filename, "image/png"}
}

// decodePNG 解出三元组里的图片，失败即 Fatal。
func decodePNG(t *testing.T, tuple [3]string) image.Image {
	t.Helper()
	payload, err := base64.StdEncoding.DecodeString(tuple[0])
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode image: %v", err)
	}
	if format != "png" {
		t.Fatalf("format = %q, want png", format)
	}
	return decoded
}

// alphaAt 取像素的 alpha（0..255）。
func alphaAt(img image.Image, x, y int) uint8 {
	_, _, _, alpha := img.At(x, y).RGBA()
	return uint8(alpha >> 8)
}

// solidImage 造一张纯色 RGBA 图。
func solidImage(width, height int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// solidGray 造一张纯灰图（PIL 的 "L" 模式）。
func solidGray(width, height int, value uint8) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetGray(x, y, color.Gray{Y: value})
		}
	}
	return img
}

// 场景 1：无 mask → 原样返回（Python 直接 `return images`，不做任何拷贝/重编码）。
func TestCompositeMaskWithoutMasksReturnsInput(t *testing.T) {
	images := [][3]string{
		{base64.StdEncoding.EncodeToString([]byte("not-an-image")), "a.png", "image/png"},
		{"", "b.png", "image/jpeg"},
	}

	got, err := CompositeMask(images, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(images) {
		t.Fatalf("len = %d, want %d", len(got), len(images))
	}
	for index := range images {
		if got[index] != images[index] {
			t.Errorf("image %d mutated: %v, want %v", index, got[index], images[index])
		}
	}

	// 空切片同样走「无 mask」分支。
	empty, err := CompositeMask(images, [][3]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(empty) != len(images) || empty[0] != images[0] {
		t.Errorf("empty masks should return input unchanged, got %v", empty)
	}
}

// 场景 2：1 图 1 mask → 合成后 alpha 通道存在且尺寸与原图一致。
//
// mask 左半透明（= 要编辑）、右半不透明（= 保留）。
func TestCompositeMaskAppliesAlphaChannel(t *testing.T) {
	source := solidImage(4, 2, color.NRGBA{R: 10, G: 20, B: 30, A: 255})

	mask := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			alpha := uint8(0) // 左半：透明 → 需要编辑
			if x >= 2 {
				alpha = 255 // 右半：不透明 → 保留
			}
			mask.SetNRGBA(x, y, color.NRGBA{R: 0, G: 0, B: 0, A: alpha})
		}
	}

	images := [][3]string{encodePNG(t, source, "photo.png")}
	masks := [][3]string{encodePNG(t, mask, "mask.png")}

	got, err := CompositeMask(images, masks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}

	// filename 保留原图名，mime 固定 image/png（Python 的第三个字段是常量）。
	if got[0][1] != "photo.png" {
		t.Errorf("filename = %q, want photo.png", got[0][1])
	}
	if got[0][2] != "image/png" {
		t.Errorf("mime = %q, want image/png", got[0][2])
	}
	if got[0][0] == images[0][0] {
		t.Error("output should be re-encoded, not the original payload")
	}

	composited := decodePNG(t, got[0])
	if bounds := composited.Bounds(); bounds.Dx() != 4 || bounds.Dy() != 2 {
		t.Fatalf("size = %dx%d, want 4x2", bounds.Dx(), bounds.Dy())
	}
	// alpha 通道必须真的来自 mask：左半 0，右半 255。
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			want := uint8(0)
			if x >= 2 {
				want = 255
			}
			if gotAlpha := alphaAt(composited, x, y); gotAlpha != want {
				t.Errorf("alpha at (%d,%d) = %d, want %d", x, y, gotAlpha, want)
			}
		}
	}
	// RGB 必须保持不变（合成只动 alpha 通道）。
	// 用 NRGBA 取非预乘值：alpha=0 时 RGBA() 会把 RGB 也压成 0，测不出差异。
	rgb := color.NRGBAModel.Convert(composited.At(0, 0)).(color.NRGBA)
	if rgb.R != 10 || rgb.G != 20 || rgb.B != 30 {
		t.Errorf("rgb at (0,0) = %d,%d,%d, want 10,20,30", rgb.R, rgb.G, rgb.B)
	}
}

// 场景 3：mask 数量少于图片 → 多出来的图回退到最后一个 mask。
//
// Python: `masks[i][0] if i < len(masks) else masks[-1][0]`
func TestCompositeMaskFallsBackToLastMask(t *testing.T) {
	first := solidImage(2, 1, color.NRGBA{A: 255})
	second := solidImage(2, 1, color.NRGBA{A: 255})

	// mask1 全不透明（255），mask2 全透明（0）。
	mask1 := encodePNG(t, solidGray(2, 1, 255), "mask1.png")
	mask2 := encodePNG(t, solidGray(2, 1, 0), "mask2.png")

	images := [][3]string{
		encodePNG(t, first, "one.png"),
		encodePNG(t, second, "two.png"),
	}

	got, err := CompositeMask(images, [][3]string{mask1, mask2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// 第 1 张用 mask1（不透明）→ alpha 255。
	if gotAlpha := alphaAt(decodePNG(t, got[0]), 0, 0); gotAlpha != 255 {
		t.Errorf("image 0 alpha = %d, want 255 (mask1)", gotAlpha)
	}
	// 第 2 张用 mask2（透明）→ alpha 0。
	if gotAlpha := alphaAt(decodePNG(t, got[1]), 0, 0); gotAlpha != 0 {
		t.Errorf("image 1 alpha = %d, want 0 (mask2)", gotAlpha)
	}

	// 只有 1 个 mask、2 张图：两张都必须用那唯一的 mask。
	single := [][3]string{encodePNG(t, solidGray(2, 1, 128), "only.png")}
	got, err = CompositeMask(images, single)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for index := range got {
		if gotAlpha := alphaAt(decodePNG(t, got[index]), 0, 0); gotAlpha != 128 {
			t.Errorf("image %d alpha = %d, want 128 (fallback mask)", index, gotAlpha)
		}
	}
}

// 场景 4：非 RGBA 的 mask（灰度 L）也能处理 —— 灰度值直接当 alpha。
func TestCompositeMaskAcceptsGrayscaleMask(t *testing.T) {
	source := solidImage(3, 1, color.NRGBA{R: 1, G: 2, B: 3, A: 255})
	mask := image.NewGray(image.Rect(0, 0, 3, 1))
	mask.SetGray(0, 0, color.Gray{Y: 0})
	mask.SetGray(1, 0, color.Gray{Y: 128})
	mask.SetGray(2, 0, color.Gray{Y: 255})

	got, err := CompositeMask(
		[][3]string{encodePNG(t, source, "photo.png")},
		[][3]string{encodePNG(t, mask, "mask.png")},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	composited := decodePNG(t, got[0])
	for x, want := range []uint8{0, 128, 255} {
		if gotAlpha := alphaAt(composited, x, 0); gotAlpha != want {
			t.Errorf("alpha at (%d,0) = %d, want %d", x, gotAlpha, want)
		}
	}
}

// mask 尺寸与原图不同时，alpha 必须被缩放到原图尺寸（Python: alpha.resize(img.size)）。
func TestCompositeMaskResizesMaskToImageSize(t *testing.T) {
	source := solidImage(4, 4, color.NRGBA{A: 255})
	// mask 只有 2x2：左列 0（编辑）、右列 255（保留）。
	mask := image.NewGray(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		mask.SetGray(0, y, color.Gray{Y: 0})
		mask.SetGray(1, y, color.Gray{Y: 255})
	}

	got, err := CompositeMask(
		[][3]string{encodePNG(t, source, "photo.png")},
		[][3]string{encodePNG(t, mask, "mask.png")},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	composited := decodePNG(t, got[0])
	if bounds := composited.Bounds(); bounds.Dx() != 4 || bounds.Dy() != 4 {
		t.Fatalf("size = %dx%d, want 4x4 (mask must be resized to the image)", bounds.Dx(), bounds.Dy())
	}
	// 左半接近 0、右半接近 255；中间允许插值过渡，所以只断言两端。
	if gotAlpha := alphaAt(composited, 0, 0); gotAlpha > 32 {
		t.Errorf("alpha at (0,0) = %d, want near 0", gotAlpha)
	}
	if gotAlpha := alphaAt(composited, 3, 0); gotAlpha < 223 {
		t.Errorf("alpha at (3,0) = %d, want near 255", gotAlpha)
	}
}

// 灰度 16 位 mask（PIL 的 I;16）走 convert("L")：>255 的值截到 255。
//
// 这条是语义陷阱：Go 的 Gray16 → Gray 会按 16→8 缩放（0x1234 → 18），
// 而 PIL 直接截断（0x1234 → 255）。取错方向会让「要编辑的区域」和
// 「要保留的区域」互换。
func TestCompositeMaskHandles16BitGrayscaleMask(t *testing.T) {
	source := solidImage(2, 1, color.NRGBA{A: 255})
	mask := image.NewGray16(image.Rect(0, 0, 2, 1))
	mask.SetGray16(0, 0, color.Gray16{Y: 0x1234})
	mask.SetGray16(1, 0, color.Gray16{Y: 0x0040})

	got, err := CompositeMask(
		[][3]string{encodePNG(t, source, "photo.png")},
		[][3]string{encodePNG(t, mask, "mask.png")},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	composited := decodePNG(t, got[0])
	// 0x1234 > 255 → 255；0x0040 = 64 → 64。
	if gotAlpha := alphaAt(composited, 0, 0); gotAlpha != 255 {
		t.Errorf("alpha at (0,0) = %d, want 255 (PIL clamps >255 to 255)", gotAlpha)
	}
	if gotAlpha := alphaAt(composited, 1, 0); gotAlpha != 64 {
		t.Errorf("alpha at (1,0) = %d, want 64", gotAlpha)
	}
}

// 灰度+alpha 的 8 位 PNG 在 PIL 里是 LA：convert("L") 只取灰度、**忽略** alpha。
// Go 把它解成 NRGBA，若只看解码类型就会误取 alpha 通道。
func TestCompositeMaskTreatsGrayAlphaPNGAsGrayscale(t *testing.T) {
	source := solidImage(2, 1, color.NRGBA{A: 255})

	// 灰度 100/200、alpha 全 0 的 LA 图。Go 的 png 解码器给出 NRGBA(100,100,100,0)。
	// 若按 RGBA 取 alpha 会得到 0/0（把整张图当成「全都要编辑」），
	// 正确结果是 100/200。
	// png.Encode 对 NRGBA 只会写 color type 6，造 LA 必须手写 IHDR/IDAT。
	payload := buildGrayAlphaPNG(t, []uint8{100, 200}, []uint8{0, 0})

	got, err := CompositeMask(
		[][3]string{encodePNG(t, source, "photo.png")},
		[][3]string{{base64.StdEncoding.EncodeToString(payload), "mask.png", "image/png"}},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	composited := decodePNG(t, got[0])
	if gotAlpha := alphaAt(composited, 0, 0); gotAlpha != 100 {
		t.Errorf("alpha at (0,0) = %d, want 100 (LA takes luminance, not alpha)", gotAlpha)
	}
	if gotAlpha := alphaAt(composited, 1, 0); gotAlpha != 200 {
		t.Errorf("alpha at (1,0) = %d, want 200", gotAlpha)
	}
}

// 打不开的 mask 要报错（Python 里 PIL 抛异常 → LoggedCall 转 502），不能静默降级成整图重画。
func TestCompositeMaskErrorsOnUndecodableMask(t *testing.T) {
	images := [][3]string{encodePNG(t, solidImage(1, 1, color.NRGBA{A: 255}), "photo.png")}
	masks := [][3]string{{base64.StdEncoding.EncodeToString([]byte("not a png")), "mask.png", "image/png"}}

	if _, err := CompositeMask(images, masks); err == nil {
		t.Fatal("expected an error for an undecodable mask")
	}
}

// 打不开的原图同样要报错。
func TestCompositeMaskErrorsOnUndecodableImage(t *testing.T) {
	images := [][3]string{{base64.StdEncoding.EncodeToString([]byte("not a png")), "photo.png", "image/png"}}
	masks := [][3]string{encodePNG(t, solidGray(1, 1, 0), "mask.png")}

	if _, err := CompositeMask(images, masks); err == nil {
		t.Fatal("expected an error for an undecodable image")
	}
}

// buildGrayAlphaPNG 手写一张 8 位灰度+alpha（color type 4）的 PNG。
//
// Go 标准库没有「灰度+alpha」的 image 类型（解码成 NRGBA），要覆盖 LA 这条
// 分支只能自己拼字节。
func buildGrayAlphaPNG(t *testing.T, luminance, alpha []uint8) []byte {
	t.Helper()
	if len(luminance) != len(alpha) {
		t.Fatalf("luminance/alpha length mismatch")
	}
	width, height := len(luminance), 1

	raw := make([]byte, 0, height*(1+width*2))
	for y := 0; y < height; y++ {
		raw = append(raw, 0) // filter type 0
		for x := 0; x < width; x++ {
			raw = append(raw, luminance[x], alpha[x])
		}
	}

	header := make([]byte, 13)
	binary.BigEndian.PutUint32(header[0:], uint32(width))
	binary.BigEndian.PutUint32(header[4:], uint32(height))
	header[8] = 8 // bit depth
	header[9] = 4 // color type 4 = grayscale + alpha
	// compression / filter / interlace 都是 0

	var out bytes.Buffer
	out.WriteString(pngSignature)
	writePNGChunk(&out, "IHDR", header)
	writePNGChunk(&out, "IDAT", zlibCompress(t, raw))
	writePNGChunk(&out, "IEND", nil)
	return out.Bytes()
}

// writePNGChunk 写一个 PNG chunk：长度 + 类型 + 数据 + CRC32。
func writePNGChunk(out *bytes.Buffer, kind string, data []byte) {
	binary.Write(out, binary.BigEndian, uint32(len(data)))
	payload := append([]byte(kind), data...)
	out.Write(payload)
	binary.Write(out, binary.BigEndian, crc32.ChecksumIEEE(payload))
}

func zlibCompress(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zlib.NewWriter(&buf)
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

// buildOversizedPNG 造一个 IHDR 声明超大尺寸、IDAT 只有几十字节的 PNG。
//
// 用来验证「解码前先按尺寸拦截」：真去解码会尝试分配 width*height*4 字节
// 并触发 `fatal error: runtime: out of memory`（运行时致命错误，
// gin.Recovery() 抓不到，整个进程退出）。文件本身只有约 100 字节。
func buildOversizedPNG(t *testing.T, width, height int) []byte {
	t.Helper()

	header := make([]byte, 13)
	binary.BigEndian.PutUint32(header[0:], uint32(width))
	binary.BigEndian.PutUint32(header[4:], uint32(height))
	header[8] = 8 // bit depth
	header[9] = 0 // color type 0 = grayscale（每像素 1 字节，解码缓冲最小）

	var out bytes.Buffer
	out.WriteString(pngSignature)
	writePNGChunk(&out, "IHDR", header)
	writePNGChunk(&out, "IDAT", zlibCompress(t, make([]byte, 32)))
	writePNGChunk(&out, "IEND", nil)
	return out.Bytes()
}

// 解压炸弹防护：IHDR 声明 40000x40000（16 亿像素）的 100 字节 PNG
// 必须在解码前被拦下，返回 error 而不是让进程 OOM。
//
// 回归点：这是 mask 合成引入的新攻击面 —— 基线只做 DecodeConfig，
// 从不对用户字节完整解码。
func TestCompositeMaskRejectsDecompressionBombImage(t *testing.T) {
	bomb := buildOversizedPNG(t, 40000, 40000)

	// 先确认这个 fixture 确实是「头部声明超大」而不是解码失败。
	config, _, err := image.DecodeConfig(bytes.NewReader(bomb))
	if err != nil {
		t.Fatalf("fixture should have a readable header: %v", err)
	}
	if config.Width != 40000 || config.Height != 40000 {
		t.Fatalf("fixture header = %dx%d, want 40000x40000", config.Width, config.Height)
	}

	images := [][3]string{{base64.StdEncoding.EncodeToString(bomb), "bomb.png", "image/png"}}
	masks := [][3]string{encodePNG(t, solidGray(1, 1, 0), "mask.png")}

	_, err = CompositeMask(images, masks)
	if err == nil {
		t.Fatal("expected an error for an oversized image; decoding it would OOM the process")
	}
	if !strings.Contains(err.Error(), "exceed") {
		t.Errorf("error should mention the pixel limit, got: %v", err)
	}
}

// mask 侧同样要拦：超大的 mask 也是用户可控输入。
func TestCompositeMaskRejectsDecompressionBombMask(t *testing.T) {
	bomb := buildOversizedPNG(t, 40000, 40000)

	images := [][3]string{encodePNG(t, solidImage(2, 2, color.NRGBA{A: 255}), "photo.png")}
	masks := [][3]string{{base64.StdEncoding.EncodeToString(bomb), "bomb.png", "image/png"}}

	_, err := CompositeMask(images, masks)
	if err == nil {
		t.Fatal("expected an error for an oversized mask; decoding it would OOM the process")
	}
	if !strings.Contains(err.Error(), "exceed") {
		t.Errorf("error should mention the pixel limit, got: %v", err)
	}
}

// 刚好在阈值内的图必须放行（上限不能定得过紧）。
//
// 89_478_485 像素上限下，9464x9464 = 89_567_296 超限，
// 9459x9459 = 89_472_681 通过。用 1x1 验证「小图照常工作」即可，
// 边界值本身由 maxImagePixels 常量表达。
func TestCompositeMaskAcceptsSmallImages(t *testing.T) {
	images := [][3]string{encodePNG(t, solidImage(1, 1, color.NRGBA{A: 255}), "photo.png")}
	masks := [][3]string{encodePNG(t, solidGray(1, 1, 128), "mask.png")}

	got, err := CompositeMask(images, masks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alpha := alphaAt(decodePNG(t, got[0]), 0, 0); alpha != 128 {
		t.Errorf("alpha = %d, want 128", alpha)
	}
}

// 记录缩放次数，用于断言「尺寸相同时不走 Scale」。
type countingScaler struct {
	inner draw.Scaler
	calls int
}

func (c *countingScaler) Scale(dst draw.Image, dr image.Rectangle, src image.Image, sr image.Rectangle, op draw.Op, opts *draw.Options) {
	c.calls++
	c.inner.Scale(dst, dr, src, sr, op, opts)
}

// mask 与原图同尺寸时必须跳过缩放。
//
// 回归点：CatmullRom 在 1:1 时是恒等映射（0 差异），但 2048x2048 仍占
// 约 43% 的合成耗时。OpenAI 规范要求 mask 与原图同尺寸，绝大多数请求
// 都在做这个无用功。
func TestCompositeMaskSkipsScalingWhenSizesMatch(t *testing.T) {
	counter := &countingScaler{inner: draw.CatmullRom}
	original := maskScaler
	maskScaler = counter
	defer func() { maskScaler = original }()

	images := [][3]string{encodePNG(t, solidImage(8, 6, color.NRGBA{A: 255}), "photo.png")}
	masks := [][3]string{encodePNG(t, solidGray(8, 6, 200), "mask.png")}

	got, err := CompositeMask(images, masks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counter.calls != 0 {
		t.Errorf("scaler called %d times for a same-size mask, want 0", counter.calls)
	}
	// 跳过缩放不能改变结果：alpha 仍然是 mask 的灰度值。
	if alpha := alphaAt(decodePNG(t, got[0]), 0, 0); alpha != 200 {
		t.Errorf("alpha = %d, want 200", alpha)
	}
}

// 尺寸不同时必须真的走缩放（上面的捷径不能把这条路径吃掉）。
func TestCompositeMaskScalesWhenSizesDiffer(t *testing.T) {
	counter := &countingScaler{inner: draw.CatmullRom}
	original := maskScaler
	maskScaler = counter
	defer func() { maskScaler = original }()

	images := [][3]string{encodePNG(t, solidImage(8, 6, color.NRGBA{A: 255}), "photo.png")}
	masks := [][3]string{encodePNG(t, solidGray(4, 3, 200), "mask.png")}

	if _, err := CompositeMask(images, masks); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counter.calls != 1 {
		t.Errorf("scaler called %d times, want 1", counter.calls)
	}
}

// 阈值必须钉在 Pillow 的硬错误边界（2 × MAX_IMAGE_PIXELS = 178_956_970），
// 既不能过紧（误伤合法大图）也不能过松（放过炸弹）。
//
// 复审实测过：把 maxImagePixels 改成 1_000，全套测试仍绿 —— 说明没有测试
// 锁住「合法大图不被误伤」。这里用 IHDR 声明尺寸来锁边界：fixture 只有
// 一百多字节，DecodeConfig 读到假尺寸，尺寸检查先返回，不会真的解码。
func TestCompositeMaskPixelLimitBoundary(t *testing.T) {
	// 用例按**当前生效的预算**判定，而不是写死 wantRejected：
	// CHATGPT2API_MAX_IMAGE_PIXELS 可以覆盖预算，写死会让测试在
	// 覆盖场景下误报。断言的是「与预算一致」这个**关系**。
	cases := []struct {
		name   string
		width  int
		height int
	}{
		{name: "2000x2000", width: 2000, height: 2000},     // 4M
		{name: "4096x4096", width: 4096, height: 4096},     // 16.7M
		{name: "12000x12000", width: 12000, height: 12000}, // 144M
		{name: "14000x14000", width: 14000, height: 14000}, // 196M
	}

	if maxImagePixels <= 0 {
		t.Fatalf("maxImagePixels = %d，必须是正数", maxImagePixels)
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// fixture 只声明尺寸，IDAT 只有 32 字节。
			oversized := buildOversizedPNG(t, tc.width, tc.height)

			// 直接测尺寸检查：不经过解码，所以不会分配 tc.width*tc.height 的内存。
			err := checkImageDimensions(oversized, "image")
			rejected := err != nil && strings.Contains(err.Error(), "exceed")

			pixels := int64(tc.width) * int64(tc.height)
			wantRejected := pixels > maxImagePixels

			if rejected != wantRejected {
				t.Fatalf("%dx%d (%d px): rejected = %v, want %v（预算 %d）",
					tc.width, tc.height, pixels, rejected, wantRejected, maxImagePixels)
			}
			if err != nil && !rejected {
				t.Fatalf("%dx%d: unexpected non-size error: %v", tc.width, tc.height, err)
			}
		})
	}
}

// mask 侧与图片侧共用同一个上限，这里确认走 mask 标签时也按同一阈值判定。
func TestCompositeMaskPixelLimitAppliesToMask(t *testing.T) {
	// 尺寸按当前生效预算选：预算足够时才验证放行（环境变量可覆盖预算）。
	if maxImagePixels >= 4_000_000 {
		withinBudget := buildOversizedPNG(t, 2000, 2000) // 4M
		if err := checkImageDimensions(withinBudget, "mask image"); err != nil {
			t.Errorf("2000x2000 mask 在预算内（%d），应放行，got: %v", maxImagePixels, err)
		}
	}
	overBudget := buildOversizedPNG(t, 12000, 12000)
	if err := checkImageDimensions(overBudget, "mask image"); err == nil {
		t.Error("12000x12000 mask 超出预算，应被拒")
	}

	overLimit := buildOversizedPNG(t, 14000, 14000)
	if err := checkImageDimensions(overLimit, "mask image"); err == nil {
		t.Error("14000x14000 mask should be rejected by the size check")
	}
}

// mask 合成必须有并发闸门。
//
// 回归点：合成发生在号池配额与槽位准入**之前**，不受任何已有并发限制约束。
// 没有闸门时 N 个并发请求就是 N × 271 MiB 的瞬时分配 —— 评审实测 3 个并发
// 把 512 MiB 的 cgroup 钉在 MemoryMax 上，两个半小时一个都没完成。
func TestCompositeGateLimitsConcurrency(t *testing.T) {
	if maxConcurrentComposites < 1 {
		t.Fatalf("闸门容量 %d，必须 >= 1", maxConcurrentComposites)
	}
	if cap(compositeGate) != maxConcurrentComposites {
		t.Fatalf("闸门容量 %d 与配置 %d 不一致", cap(compositeGate), maxConcurrentComposites)
	}

	// 取满全部许可后，下一次获取必须阻塞 —— 证明闸门真的在限流。
	releases := make([]func(), 0, maxConcurrentComposites)
	for i := 0; i < maxConcurrentComposites; i++ {
		releases = append(releases, acquireComposite())
	}

	blocked := make(chan struct{})
	go func() {
		rel := acquireComposite()
		close(blocked)
		rel()
	}()

	select {
	case <-blocked:
		t.Fatal("闸门已满却仍放行 —— 并发限制未生效")
	case <-time.After(60 * time.Millisecond):
		// 正确：闸门满了，第三次获取在等待
	}

	// 释放一个后应立刻放行
	releases[0]()
	select {
	case <-blocked:
		// 正确
	case <-time.After(time.Second):
		t.Fatal("释放许可后仍未放行")
	}
	for _, rel := range releases[1:] {
		rel()
	}

	// 闸门必须回到可用状态（否则第一次用过就永久堵死）
	rel := acquireComposite()
	rel()
	t.Logf("闸门容量 %d，限流与释放均正常", maxConcurrentComposites)
}
