package protocol

import "testing"

func TestSplitImageModel(t *testing.T) {
	cases := []struct {
		input    string
		planType string
		base     string
	}{
		{"gpt-image-2", "", "gpt-image-2"},
		{"codex-gpt-image-2", "", "codex-gpt-image-2"},
		{"team-codex-gpt-image-2", "team", "codex-gpt-image-2"},
		{"plus-codex-gpt-image-2", "plus", "codex-gpt-image-2"},
		{"pro-codex-gpt-image-2", "pro", "codex-gpt-image-2"},
		{"GPT-IMAGE-2", "", "gpt-image-2"},
		{"unknown-model", "", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		plan, base := SplitImageModel(tc.input)
		if plan != tc.planType || base != tc.base {
			t.Errorf("SplitImageModel(%q) = (%q,%q), want (%q,%q)",
				tc.input, plan, base, tc.planType, tc.base)
		}
	}
}

func TestIsSupportedImageModel(t *testing.T) {
	for _, model := range []string{"gpt-image-2", "codex-gpt-image-2", "team-codex-gpt-image-2"} {
		if !IsSupportedImageModel(model) {
			t.Errorf("%q should be supported", model)
		}
	}
	for _, model := range []string{"gpt-5", "auto", "dall-e-3", ""} {
		if IsSupportedImageModel(model) {
			t.Errorf("%q should not be supported", model)
		}
	}
}

func TestIsCodexImageModel(t *testing.T) {
	if !IsCodexImageModel("codex-gpt-image-2") {
		t.Error("codex-gpt-image-2 should be a codex model")
	}
	if !IsCodexImageModel("team-codex-gpt-image-2") {
		t.Error("prefixed codex model should be a codex model")
	}
	if IsCodexImageModel("gpt-image-2") {
		t.Error("gpt-image-2 is not a codex model")
	}
}

// 空号池时不应动态追加任何图片模型（Python 版 web_image_accounts 为空则不加）。
func TestListModelsEmptyPool(t *testing.T) {
	result := ListModelsResult(nil, nil)
	data, _ := result["data"].([]map[string]any)
	if len(data) != 0 {
		t.Fatalf("empty pool should produce no models, got %d", len(data))
	}
}

// web 账号存在时加 gpt-image-2，但不加 codex 系列。
func TestListModelsWebOnly(t *testing.T) {
	result := ListModelsResult(nil, []AccountSummary{{Type: "Plus", SourceType: "web"}})
	ids := modelIDs(result)
	if !ids["gpt-image-2"] {
		t.Error("web account should expose gpt-image-2")
	}
	if ids["codex-gpt-image-2"] {
		t.Error("web Plus account must not expose codex-gpt-image-2")
	}
}

// codex Team 账号加 codex-gpt-image-2 与 team- 前缀，不加 plus/pro。
func TestListModelsCodexTeam(t *testing.T) {
	result := ListModelsResult(nil, []AccountSummary{
		{Type: "Team", SourceType: "codex"},
	})
	ids := modelIDs(result)
	if !ids["codex-gpt-image-2"] {
		t.Error("codex Team should expose codex-gpt-image-2")
	}
	if !ids["team-codex-gpt-image-2"] {
		t.Error("codex Team should expose team- prefix")
	}
	if ids["plus-codex-gpt-image-2"] || ids["pro-codex-gpt-image-2"] {
		t.Error("codex Team must not expose plus/pro prefixes")
	}
}

// 上游已有同名模型时不重复追加。
func TestListModelsDeduplicates(t *testing.T) {
	result := ListModelsResult(
		[]map[string]any{{"id": "gpt-image-2", "object": "model", "owned_by": "openai"}},
		[]AccountSummary{{Type: "Plus", SourceType: "web"}},
	)
	data, _ := result["data"].([]map[string]any)
	count := 0
	for _, item := range data {
		if stringOf(item["id"]) == "gpt-image-2" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("gpt-image-2 appears %d times, want 1", count)
	}
}

func modelIDs(result map[string]any) map[string]bool {
	out := map[string]bool{}
	data, _ := result["data"].([]map[string]any)
	for _, item := range data {
		out[stringOf(item["id"])] = true
	}
	return out
}
