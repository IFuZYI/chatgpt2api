package config_test

import (
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/config"
)

// image_timeout_retry_secs 必须被校验并归一化。
//
// 它由前端「图片超时继续等待时间」使用：用户点「继续等待」时，
// 前端把这个值作为 extra_timeout_secs 发给后端（后端会拒绝 <5 或 >120），
// 所以非法值会让那次操作直接 400 —— 而设置页保存时却毫无提示。
//
// 审计实测：保存 -999 会被原样接受并落盘。
func TestImageTimeoutRetrySecsIsValidated(t *testing.T) {
	store, _ := newStore(t, nil)

	if _, err := store.Update(map[string]any{"image_timeout_retry_secs": -999}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := store.ImageTimeoutRetrySecs()
	if got < 5 || got > 120 {
		t.Errorf("ImageTimeoutRetrySecs() = %d, 应被钳制到 [5,120]", got)
	}
}

// 合法值必须原样保留（不能把用户设置改掉）。
func TestImageTimeoutRetrySecsKeepsValidValue(t *testing.T) {
	store, _ := newStore(t, nil)
	if _, err := store.Update(map[string]any{"image_timeout_retry_secs": 45}); err != nil {
		t.Fatal(err)
	}
	if got := store.ImageTimeoutRetrySecs(); got != 45 {
		t.Errorf("= %d, want 45", got)
	}
}

// 缺省时用 30（与前端 fallback 和 resume-poll 的默认值一致）。
func TestImageTimeoutRetrySecsDefault(t *testing.T) {
	store, _ := newStore(t, nil)
	if got := store.ImageTimeoutRetrySecs(); got != 30 {
		t.Errorf("默认 = %d, want 30", got)
	}
}

// 落盘值也必须是归一化后的，不能只靠读取时钳制。
func TestImageTimeoutRetrySecsPersistsNormalized(t *testing.T) {
	store, dir := newStore(t, nil)
	if _, err := store.Update(map[string]any{"image_timeout_retry_secs": 100000}); err != nil {
		t.Fatal(err)
	}
	// 重新打开，确认磁盘上存的就是合法值
	reopened, err := config.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.ImageTimeoutRetrySecs(); got != 120 {
		t.Errorf("重新加载后 = %d, want 120（越界应被钳制）", got)
	}
}
