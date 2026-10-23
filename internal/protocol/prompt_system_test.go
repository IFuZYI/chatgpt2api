package protocol

import "testing"

// 全局系统提示词必须前置拼进 prompt。
//
// 回归点：config.global_system_prompt 保存成功但请求链路从不读取，
// 用户以为统一画风/约束已生效，实际零效果。
func TestPromptWithGlobalSystem(t *testing.T) {
	if got := PromptWithGlobalSystem("a cat", ""); got != "a cat" {
		t.Errorf("empty system should pass through, got %q", got)
	}
	if got := PromptWithGlobalSystem("a cat", "   "); got != "a cat" {
		t.Errorf("whitespace-only system should pass through, got %q", got)
	}
	got := PromptWithGlobalSystem("a cat", "always watercolor")
	want := "always watercolor\n\na cat"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
