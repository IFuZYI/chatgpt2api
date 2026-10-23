package protocol

import (
	"encoding/base64"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// 图片 token 计费常量，逐字取自 utils/image_tokens.py。
const (
	defaultImageWidth    = 1024
	defaultImageHeight   = 1024
	patchSize            = 32
	tileSize             = 512
	tileHighShortSide    = 768
	imageInputTokenModel = "gpt-5.4-mini"
)

// patch1536Models 是走 1536/2048 patch 预算的模型前缀。
var patch1536Models = []string{
	"gpt-5.4-mini", "gpt-5.4-nano", "gpt-5-mini", "gpt-5-nano",
	"gpt-5.2", "gpt-5.3-codex", "gpt-5-codex-mini", "gpt-5.1-codex-mini",
	"gpt-5.2-codex", "gpt-5.2-chat-latest", "o4-mini", "gpt-4.1-mini", "gpt-4.1-nano",
}

var patchMultipliers = map[string]float64{
	"gpt-5.4-mini": 1.62,
	"gpt-5.4-nano": 2.46,
	"gpt-5-mini":   1.62,
	"gpt-5-nano":   2.46,
	"gpt-4.1-mini": 1.62,
	"gpt-4.1-nano": 2.46,
	"o4-mini":      1.72,
}

var sizePattern = regexp.MustCompile(`(\d{2,5})\D+(\d{2,5})`)

func modelName(model string) string { return strings.ToLower(strings.TrimSpace(model)) }

// ParseImageSize 复刻 parse_image_size：支持 "1024x1024"、"1024 1024"、数组。
func ParseImageSize(size any) (int, int) {
	switch typed := size.(type) {
	case []any:
		if len(typed) >= 2 {
			width := intValueOf(typed[0])
			height := intValueOf(typed[1])
			if width > 0 && height > 0 {
				return width, height
			}
		}
	case []int:
		if len(typed) >= 2 && typed[0] > 0 && typed[1] > 0 {
			return typed[0], typed[1]
		}
	}
	text := ""
	if size != nil {
		text = stringOf(size)
	}
	match := sizePattern.FindStringSubmatch(text)
	if len(match) < 3 {
		return defaultImageWidth, defaultImageHeight
	}
	width, _ := strconv.Atoi(match[1])
	height, _ := strconv.Atoi(match[2])
	if width <= 0 || height <= 0 {
		return defaultImageWidth, defaultImageHeight
	}
	return width, height
}

func patchCount(width, height float64) int {
	return int(math.Ceil(width/patchSize)) * int(math.Ceil(height/patchSize))
}

func patchMultiplier(model string) float64 {
	name := modelName(model)
	for prefix, multiplier := range patchMultipliers {
		if strings.HasPrefix(name, prefix) {
			return multiplier
		}
	}
	return 1.0
}

func patchLimits(model, detail string) (int, int, bool) {
	name := modelName(model)
	for _, prefix := range patch1536Models {
		if strings.HasPrefix(name, prefix) {
			return 1536, 2048, true
		}
	}
	if strings.HasPrefix(name, "gpt-5.5") {
		if detail == "auto" || detail == "original" {
			return 10000, 6000, true
		}
		return 2500, 2048, true
	}
	if strings.HasPrefix(name, "gpt-5.4") {
		if detail == "original" {
			return 10000, 6000, true
		}
		return 2500, 2048, true
	}
	return 0, 0, false
}

func patchTokens(width, height int, model, detail string) int {
	multiplier := patchMultiplier(model)
	if detail == "low" {
		return int(math.Ceil(256 * multiplier))
	}
	patchBudget, maxDimension, ok := patchLimits(model, detail)
	if !ok {
		return 0
	}
	scale := math.Min(1.0, float64(maxDimension)/math.Max(float64(width), float64(height)))
	resizedWidth := float64(width) * scale
	resizedHeight := float64(height) * scale

	if patchCount(resizedWidth, resizedHeight) > patchBudget {
		shrinkFactor := math.Sqrt(float64(patchSize*patchSize*patchBudget) / (resizedWidth * resizedHeight))
		widthUnits := resizedWidth * shrinkFactor / patchSize
		heightUnits := resizedHeight * shrinkFactor / patchSize
		adjusted := shrinkFactor * math.Min(
			safeFloorRatio(widthUnits),
			safeFloorRatio(heightUnits),
		)
		resizedWidth *= adjusted
		resizedHeight *= adjusted
	}

	tokens := patchCount(math.Max(1, resizedWidth), math.Max(1, resizedHeight))
	if tokens > patchBudget {
		tokens = patchBudget
	}
	return int(math.Ceil(float64(tokens) * multiplier))
}

func safeFloorRatio(units float64) float64 {
	if units == 0 {
		return 1
	}
	return math.Floor(units) / units
}

func tileRates(model string) (int, int) {
	name := modelName(model)
	switch {
	case name == "gpt-5" || name == "gpt-5-chat-latest":
		return 70, 140
	case strings.HasPrefix(name, "gpt-4o-mini"):
		return 2833, 5667
	case strings.HasPrefix(name, "o1"), strings.HasPrefix(name, "o1-pro"), strings.HasPrefix(name, "o3"):
		return 75, 150
	case strings.HasPrefix(name, "computer-use-preview"):
		return 65, 129
	default:
		return 85, 170
	}
}

func tileTokens(width, height int, model, detail string) int {
	baseTokens, perTile := tileRates(model)
	if detail == "low" {
		return baseTokens
	}
	scale := math.Min(1.0, 2048/math.Max(float64(width), float64(height)))
	resizedWidth := float64(width) * scale
	resizedHeight := float64(height) * scale
	shortSide := math.Min(resizedWidth, resizedHeight)
	if shortSide > 0 {
		adjust := tileHighShortSide / shortSide
		resizedWidth *= adjust
		resizedHeight *= adjust
	}
	tiles := int(math.Ceil(resizedWidth/tileSize)) * int(math.Ceil(resizedHeight/tileSize))
	return baseTokens + tiles*perTile
}

// CountImageInputTokens 复刻 count_image_input_tokens。
//
// ⚠️ 注意：Python 版**忽略传入的 model**，永远用常量 IMAGE_INPUT_TOKEN_MODEL
// （"gpt-5.4-mini"）算。所以 gpt-4o 的图与 gpt-5.5 的图计费完全一样 ——
// 这是上游的计费口径，不是笔误。按调用方 model 分支会得到不同的数字（实测
// 1024x1024 auto：正确 1659，按 model 分支会算出 1024）。
//
// input_fidelity 参数在 Python 侧声明了但没被使用，这里同样忽略。
func CountImageInputTokens(width, height int, model, detail string) int {
	if width <= 0 || height <= 0 {
		return 0
	}
	_ = model // 刻意不用；见上面的说明
	normalizedDetail := strings.ToLower(strings.TrimSpace(detail))
	if normalizedDetail == "" {
		normalizedDetail = "auto"
	}
	return patchTokens(width, height, imageInputTokenModel, normalizedDetail)
}

// CountImageInputsTokens 对一组 (bytes, filename, mime) 求和。
func CountImageInputsTokens(images [][3]string, model string, detail string) int {
	total := 0
	for _, image := range images {
		payload, err := base64.StdEncoding.DecodeString(image[0])
		if err != nil {
			continue
		}
		width, height, ok := imageSizeFromBytes(payload)
		if !ok {
			continue
		}
		total += CountImageInputTokens(width, height, model, detail)
	}
	return total
}

// CountGeneratedImageTokens 复刻 count_generated_image_tokens。
func CountGeneratedImageTokens(width, height int, quality string) int {
	patches := float64(patchCount(float64(width), float64(height)))
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "low":
		return int(math.Ceil(patches * 17 / 64))
	case "high", "hd":
		return int(math.Ceil(patches * 65 / 16))
	default:
		return int(math.Ceil(patches * 33 / 32))
	}
}

// CountImageOutputTokens 复刻 count_image_output_tokens。
func CountImageOutputTokens(size any, quality string, count int) int {
	width, height := ParseImageSize(size)
	if count < 0 {
		count = 0
	}
	return count * CountGeneratedImageTokens(width, height, quality)
}

// CountImageOutputItemsTokens 优先用实际图片尺寸，缺失时回退请求的 size。
func CountImageOutputItemsTokens(items []map[string]any, size any, quality string) int {
	if len(items) == 0 {
		return 0
	}
	fallbackWidth, fallbackHeight := ParseImageSize(size)
	total := 0
	for _, item := range items {
		width, height := fallbackWidth, fallbackHeight
		if encoded, ok := item["b64_json"].(string); ok && strings.TrimSpace(encoded) != "" {
			if payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded)); err == nil {
				if actualWidth, actualHeight, ok := imageSizeFromBytes(payload); ok {
					width, height = actualWidth, actualHeight
				}
			}
		}
		total += CountGeneratedImageTokens(width, height, quality)
	}
	return total
}

// TokenUsage 复刻 token_usage 的返回结构（字段名是 /v1/responses 的契约）。
func TokenUsage(inputTextTokens, inputImageTokens, outputTextTokens, outputImageTokens int) map[string]any {
	inputTokens := maxIntValue(inputTextTokens, 0) + maxIntValue(inputImageTokens, 0)
	outputTokens := maxIntValue(outputTextTokens, 0) + maxIntValue(outputImageTokens, 0)
	return map[string]any{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"total_tokens":  inputTokens + outputTokens,
		"input_tokens_details": map[string]any{
			"text_tokens":   maxIntValue(inputTextTokens, 0),
			"image_tokens":  maxIntValue(inputImageTokens, 0),
			"cached_tokens": 0,
		},
		"output_tokens_details": map[string]any{
			"text_tokens":      maxIntValue(outputTextTokens, 0),
			"image_tokens":     maxIntValue(outputImageTokens, 0),
			"reasoning_tokens": 0,
		},
	}
}

// ImageUsage 复刻 image_usage：输出只算图片。
func ImageUsage(inputTextTokens, inputImageTokens, outputTokens int) map[string]any {
	return TokenUsage(inputTextTokens, inputImageTokens, 0, outputTokens)
}

// ChatUsageFromImageUsage 把 Responses 形态的 usage 转成 Chat Completions 形态。
func ChatUsageFromImageUsage(usage map[string]any) map[string]any {
	inputTokens := intValueOf(usage["input_tokens"])
	outputTokens := intValueOf(usage["output_tokens"])
	inputDetails, _ := usage["input_tokens_details"].(map[string]any)
	outputDetails, _ := usage["output_tokens_details"].(map[string]any)
	return map[string]any{
		"prompt_tokens":     inputTokens,
		"completion_tokens": outputTokens,
		"total_tokens":      inputTokens + outputTokens,
		"prompt_tokens_details": map[string]any{
			"text_tokens":   intValueOf(inputDetails["text_tokens"]),
			"image_tokens":  intValueOf(inputDetails["image_tokens"]),
			"cached_tokens": intValueOf(inputDetails["cached_tokens"]),
		},
		"completion_tokens_details": map[string]any{
			"text_tokens":      intValueOf(outputDetails["text_tokens"]),
			"image_tokens":     intValueOf(outputDetails["image_tokens"]),
			"reasoning_tokens": intValueOf(outputDetails["reasoning_tokens"]),
		},
	}
}

func maxIntValue(a, b int) int {
	if a > b {
		return a
	}
	return b
}
