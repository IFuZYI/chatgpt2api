package upstream

import "testing"

// gpt-image-2 的底层上游模型必须来自配置，不能被硬编码兜底吃掉。
func TestImageModelSettingsUsesInjectedDefault(t *testing.T) {
	c := &Client{}
	upstream, effort := c.imageModelSettings("gpt-image-2", "gpt-5-5-extended", "max")
	if upstream != "gpt-5-5" {
		t.Errorf("upstream = %q, want gpt-5-5 (suffix stripped)", upstream)
	}
	if effort != "extended" {
		t.Errorf("effort = %q, want extended (from suffix)", effort)
	}

	// 空配置时才兜底
	upstream, _ = c.imageModelSettings("gpt-image-2", "", "")
	if upstream != "gpt-5-5" {
		t.Errorf("empty default should fall back to gpt-5-5, got %q", upstream)
	}
}
