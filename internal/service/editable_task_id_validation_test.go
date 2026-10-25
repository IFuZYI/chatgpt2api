package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// client_task_id 不能逃出 data/files（任意文件写入的入口）。
func TestEditableTaskIDRejectsTraversal(t *testing.T) {
	svc, dataDir := newEditableTestServiceWithDir(t)
	root := filepath.Join(dataDir, "files")

	for _, bad := range []string{
		"../../../../escape_001",
		"..",
		"a/b",
		`a\b`,
		"a.b",
		"..%2f..",
		"",
	} {
		if bad == "" {
			continue // 空 id 是合法的（服务端生成 UUID）
		}
		if _, err := svc.Submit("attacker", bad, "k", "ppt", "p", nil, ""); err == nil {
			t.Errorf("非法 id %q 被接受，应拒绝", bad)
		}
	}

	// 确认没有任何文件被写到 root 之外
	parent := filepath.Dir(root)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if e.Name() == "files" {
			continue
		}
		if len(e.Name()) >= 10 && e.Name()[:10] == "escape_001" {
			t.Fatalf("逃逸目录被创建: %s", e.Name())
		}
	}
	t.Log("所有非法 id 均被拒绝，无越界目录")
}

// 合法 id 仍然可用（UUID 形态与自造 id）。
func TestEditableTaskIDAcceptsValid(t *testing.T) {
	svc, _ := newEditableTestServiceWithDir(t)
	svc.SetAccounts(d1FakeAccounts{})
	svc.SetRunner(func(ctx EditableFileTaskContext) (EditableFileTaskResult, error) {
		return EditableFileTaskResult{}, errors.New("fake")
	})
	for _, ok := range []string{"my-task-1", "abc_123", "550e8400-e29b-41d4-a716-446655440000"} {
		if _, err := svc.Submit("u", ok, "k", "ppt", "p", nil, ""); err != nil {
			t.Errorf("合法 id %q 被拒绝: %v", ok, err)
		}
	}
}

type d1FakeAccounts struct{}

func (d1FakeAccounts) EditableFileAccessToken() (string, error) { return "t", nil }
func (d1FakeAccounts) EditableFileAccountEmail(string) string   { return "e@x.test" }
func (d1FakeAccounts) MarkTextUsed(string)                      {}
