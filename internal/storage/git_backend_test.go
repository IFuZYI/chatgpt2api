package storage

import (
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v5"
	gkconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// newBareRemote 建一个本地 bare 仓库当远端。
//
// 为什么不用真实远端：git 后端此前**完全没有测试**（268 行实现，
// 只在 factory 里有分支）。用本地 bare 仓库就能覆盖 clone/pull/commit/push
// 这条完整链路，且不需要网络与凭据。
func newBareRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatalf("init bare repo: %v", err)
	}
	// 建一个初始提交，否则 clone 会失败（空仓库没有可检出的分支）。
	work := filepath.Join(t.TempDir(), "seed")
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatalf("init seed repo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("seed", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "t@example.test"},
	}); err != nil {
		t.Fatal(err)
	}
	// 注意 URL 是 bare 仓库 dir，不是 seed 工作区 work —— 写错会推给自己。
	if _, err := repo.CreateRemote(&gkconfig.RemoteConfig{Name: "origin", URLs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gkconfig.RefSpec{"refs/heads/master:refs/heads/main"},
	}); err != nil && err != git.NoErrAlreadyUpToDate {
		t.Fatalf("seed push: %v", err)
	}
	// bare 仓库的 HEAD 默认指向 master，而我们把种子推到了 main —— 不修正
	// HEAD 的话 clone 会报 "remote repository is empty"。
	head := filepath.Join(dir, "HEAD")
	if err := os.WriteFile(head, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("fix bare HEAD: %v", err)
	}
	return dir
}

// Git 后端必须能往返账号数据（写 → 远端 → 读回）。
//
// 复刻 Python 的 git_storage：每次读写都 clone/pull，写完 commit + push。
func TestGitBackendRoundTrip(t *testing.T) {
	remote := newBareRemote(t)
	cache := t.TempDir()
	backend, err := NewGitBackend(remote, "", "main", "accounts.json", "auth_keys.json", cache)
	if err != nil {
		t.Fatalf("NewGitBackend: %v", err)
	}

	items := []map[string]any{
		{"access_token": "at-1", "email": "a@example.test", "status": "正常"},
	}
	if err := backend.SaveAccounts(items); err != nil {
		t.Fatalf("SaveAccounts: %v", err)
	}

	// 换一个干净的 cache 目录重新读 —— 强制走 clone 路径，
	// 证明数据真的到了远端而不只是本地文件。
	fresh, err := NewGitBackend(remote, "", "main", "accounts.json", "auth_keys.json", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := fresh.LoadAccounts()
	if err != nil {
		t.Fatalf("LoadAccounts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("读回 %d 条，应为 1", len(got))
	}
	if got[0]["access_token"] != "at-1" {
		t.Errorf("access_token = %v", got[0]["access_token"])
	}
}

// 文件不存在时返回空，不是错误（新仓库还没有 accounts.json）。
func TestGitBackendMissingFileIsEmpty(t *testing.T) {
	remote := newBareRemote(t)
	backend, err := NewGitBackend(remote, "", "main", "accounts.json", "auth_keys.json", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := backend.LoadAccounts()
	if err != nil {
		t.Fatalf("缺失文件不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("应为空，实际 %d 条", len(got))
	}
}

// 缺 GIT_REPO_URL 必须报错（不是静默落到别的后端）。
func TestGitBackendRequiresRepoURL(t *testing.T) {
	if _, err := NewGitBackend("", "", "main", "", "", t.TempDir()); err == nil {
		t.Fatal("缺 GIT_REPO_URL 应报错")
	}
}
