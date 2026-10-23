package upstream

import "testing"

// 2.5 系列必须把模型名原样透传给上游，不能被 defaultUpstream 吃掉。
//
// 回归点：早期实现只有 gpt-image-2 分支，其余模型返回 "auto"，
// 导致 2.5 系列全部退化成同一个模型（甚至直接不可用）。
func TestImageModelSettings25SeriesPassThrough(t *testing.T) {
	c := &Client{}
	for _, model := range []string{
		"gpt-image-2.5",
		"gpt-image-2.5-flare",
		"gpt-image-2.5-sunburst",
	} {
		upstream, _ := c.imageModelSettings(model, "gpt-5-5", "auto")
		if upstream != model {
			t.Errorf("%q -> upstream %q, want pass-through %q", model, upstream, model)
		}
	}
}

// gpt-image-2 仍走配置的上游模型（保持原有语义）。
func TestImageModelSettingsGPTImage2UsesDefault(t *testing.T) {
	c := &Client{}
	upstream, _ := c.imageModelSettings("gpt-image-2", "gpt-5-5", "auto")
	if upstream != "gpt-5-5" {
		t.Errorf("upstream = %q, want gpt-5-5", upstream)
	}
	// 空配置时兜底 gpt-5-5
	upstream, _ = c.imageModelSettings("gpt-image-2", "", "auto")
	if upstream != "gpt-5-5" {
		t.Errorf("empty default should fall back to gpt-5-5, got %q", upstream)
	}
}

// 未收录的模型名必须返回 auto（不透传）。
func TestImageModelSettingsUnknownModel(t *testing.T) {
	c := &Client{}
	upstream, effort := c.imageModelSettings("gpt-image-99", "gpt-5-5", "auto")
	if upstream != "auto" || effort != "" {
		t.Errorf("unknown model should yield (auto, \"\"), got (%q, %q)", upstream, effort)
	}
}
