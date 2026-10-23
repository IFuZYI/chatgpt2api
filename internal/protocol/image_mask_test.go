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
	"testing"
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
