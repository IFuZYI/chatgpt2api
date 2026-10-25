package upstream

import "testing"

// gpt-image-2 的底层上游模型必须来自配置，不能被硬编码兜底吃掉。
func TestImageModelSettingsUsesInjectedDefault(t *testing.T) {
	c := &Client{}
	// 夹具用当前上游存在的模型名（gpt-5-5 已从 /backend-api/models 消失）。
	upstream, effort := c.imageModelSettings("gpt-image-2", "gpt-5-6-extended", "max")
	if upstream != "gpt-5-6" {
		t.Errorf("upstream = %q, want gpt-5-6 (suffix stripped)", upstream)
	}
	if effort != "extended" {
		t.Errorf("effort = %q, want extended (from suffix)", effort)
	}

	// 空配置时才兜底。回退值是 auto —— gpt-5-5 已从上游消失。
	upstream, _ = c.imageModelSettings("gpt-image-2", "", "")
	if upstream != "auto" {
		t.Errorf("empty default should fall back to auto, got %q", upstream)
	}
}
