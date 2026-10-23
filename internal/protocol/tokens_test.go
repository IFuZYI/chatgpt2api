package protocol

import "testing"

// 下面这些期望值全部由 Python 的 utils/image_tokens.py 实测产出，
// 不是按公式手算的。任何漂移都意味着计费数字变了。
func TestGeneratedImageTokensMatchPython(t *testing.T) {
	cases := []struct {
		width, height int
		quality       string
		want          int
	}{
		{1024, 1024, "auto", 1056},
		{1024, 1024, "low", 272},
		{1024, 1024, "high", 4160},
		{512, 512, "auto", 264},
		{2048, 2048, "auto", 4224},
		{1536, 1024, "high", 6240},
	}
	for _, tc := range cases {
		got := CountGeneratedImageTokens(tc.width, tc.height, tc.quality)
		if got != tc.want {
			t.Errorf("CountGeneratedImageTokens(%d,%d,%q) = %d, want %d",
				tc.width, tc.height, tc.quality, got, tc.want)
		}
	}
}

func TestParseImageSizeMatchesPython(t *testing.T) {
	cases := []struct {
		input      any
		wantWidth  int
		wantHeight int
	}{
		{"1024 768", 1024, 768},
		{"1024x768", 1024, 768},
		{nil, 1024, 1024},
		{"abc", 1024, 1024},
		{"", 1024, 1024},
		{[]any{512, 640}, 512, 640},
		{[]any{0, 640}, 1024, 1024},
	}
	for _, tc := range cases {
		width, height := ParseImageSize(tc.input)
		if width != tc.wantWidth || height != tc.wantHeight {
			t.Errorf("ParseImageSize(%v) = (%d,%d), want (%d,%d)",
				tc.input, width, height, tc.wantWidth, tc.wantHeight)
		}
	}
}

func TestImageInputTokensMatchPython(t *testing.T) {
	cases := []struct {
		model, detail string
		width, height int
		want          int
	}{
		{"gpt-5-mini", "auto", 1024, 1024, 1659},
		{"gpt-5.5", "auto", 1024, 1024, 1659},
		{"gpt-4o", "auto", 512, 512, 415},
		{"gpt-5", "low", 1024, 1024, 415},
		{"gpt-5.5", "low", 1024, 1024, 415},
	}
	for _, tc := range cases {
		got := CountImageInputTokens(tc.width, tc.height, tc.model, tc.detail)
		if got != tc.want {
			t.Errorf("CountImageInputTokens(%d,%d,%q,%q) = %d, want %d",
				tc.width, tc.height, tc.model, tc.detail, got, tc.want)
		}
	}
}

func TestCountImageOutputTokensMatchesPython(t *testing.T) {
	if got := CountImageOutputTokens("1024x1024", "auto", 2); got != 2112 {
		t.Errorf("CountImageOutputTokens = %d, want 2112", got)
	}
	if got := CountImageOutputTokens("1024x1024", "auto", 0); got != 0 {
		t.Errorf("zero count should be 0, got %d", got)
	}
}

// usage 结构是对外契约，字段一个都不能少。
func TestTokenUsageShape(t *testing.T) {
	usage := TokenUsage(10, 20, 30, 40)
	if usage["input_tokens"] != 30 || usage["output_tokens"] != 70 || usage["total_tokens"] != 100 {
		t.Fatalf("top-level usage wrong: %#v", usage)
	}
	inDetails, _ := usage["input_tokens_details"].(map[string]any)
	if inDetails["text_tokens"] != 10 || inDetails["image_tokens"] != 20 || inDetails["cached_tokens"] != 0 {
		t.Fatalf("input details wrong: %#v", inDetails)
	}
	outDetails, _ := usage["output_tokens_details"].(map[string]any)
	if outDetails["text_tokens"] != 30 || outDetails["image_tokens"] != 40 || outDetails["reasoning_tokens"] != 0 {
		t.Fatalf("output details wrong: %#v", outDetails)
	}
}

func TestChatUsageFromImageUsage(t *testing.T) {
	usage := ChatUsageFromImageUsage(TokenUsage(10, 20, 30, 40))
	if usage["prompt_tokens"] != 30 || usage["completion_tokens"] != 70 || usage["total_tokens"] != 100 {
		t.Fatalf("chat usage wrong: %#v", usage)
	}
	promptDetails, _ := usage["prompt_tokens_details"].(map[string]any)
	if promptDetails["image_tokens"] != 20 {
		t.Fatalf("prompt details wrong: %#v", promptDetails)
	}
}
