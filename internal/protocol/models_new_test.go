package protocol

import "testing"

// 2.5 系列必须被识别为受支持的图片模型。
//
// 这些 slug 不在上游 /backend-api/models 里（该端点只暴露文本模型），
// 只能实测确认：同 prompt 下三者产出字节数各异，确属不同模型。
func TestImageModelsInclude25Series(t *testing.T) {
	for _, model := range []string{
		"gpt-image-2",
		"gpt-image-2.5",
		"gpt-image-2.5-flare",
		"gpt-image-2.5-sunburst",
		"codex-gpt-image-2",
	} {
		if !IsSupportedImageModel(model) {
			t.Errorf("%q should be a supported image model", model)
		}
	}

	// 大小写不敏感
	for _, model := range []string{"GPT-IMAGE-2.5", "Gpt-Image-2.5-Flare"} {
		if !IsSupportedImageModel(model) {
			t.Errorf("%q should be supported (case-insensitive)", model)
		}
	}

	// 未收录的名字必须拒绝，避免把任意字符串透传给上游
	for _, model := range []string{"gpt-image-3", "gpt-image-2.5-storm", "dall-e-3"} {
		if IsSupportedImageModel(model) {
			t.Errorf("%q should not be supported", model)
		}
	}
}

// 2.5 系列不带套餐前缀，SplitImageModel 应返回 ("", 原值)。
func TestSplitImageModel25Series(t *testing.T) {
	plan, base := SplitImageModel("gpt-image-2.5-flare")
	if plan != "" {
		t.Errorf("plan = %q, want empty", plan)
	}
	if base != "gpt-image-2.5-flare" {
		t.Errorf("base = %q", base)
	}
}

// 2.6 系列必须被识别为受支持的图片模型。
//
// 上游 2026-11 发布 ChatGPT Images 2.6，API 侧同时提供 Flare / Sunburst
// 两个变体。三者与 2.5 系列一样，slug 不在 /backend-api/models 里，只能写死。
func TestImageModelsInclude26Series(t *testing.T) {
	for _, model := range []string{
		"gpt-image-2.6",
		"gpt-image-2.6-flare",
		"gpt-image-2.6-sunburst",
	} {
		if !IsSupportedImageModel(model) {
			t.Errorf("%q should be a supported image model", model)
		}
	}

	// 大小写不敏感
	for _, model := range []string{"GPT-IMAGE-2.6", "Gpt-Image-2.6-Flare"} {
		if !IsSupportedImageModel(model) {
			t.Errorf("%q should be supported (case-insensitive)", model)
		}
	}

	// 未收录的名字仍必须拒绝
	for _, model := range []string{"gpt-image-2.6-storm", "gpt-image-2.7"} {
		if IsSupportedImageModel(model) {
			t.Errorf("%q should not be supported", model)
		}
	}
}

// 2.6 系列不带套餐前缀，SplitImageModel 应返回 ("", 原值)。
func TestSplitImageModel26Series(t *testing.T) {
	plan, base := SplitImageModel("gpt-image-2.6-sunburst")
	if plan != "" {
		t.Errorf("plan = %q, want empty", plan)
	}
	if base != "gpt-image-2.6-sunburst" {
		t.Errorf("base = %q", base)
	}
}
