package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// GitBackend 复刻 services/storage/git_storage.py。
//
// 每次读写都先 clone/pull（与 Python 一致，不做本地缓存失效判断），
// 写完后 commit + push。这个实现很慢，但它是可选后端，语义正确优先。
type GitBackend struct {
	repoURL      string
	token        string
	branch       string
	accountsPath string
	authKeysPath string
	cacheDir     string
	mu           sync.Mutex
}

func NewGitBackend(repoURL, token, branch, accountsPath, authKeysPath, cacheDir string) (*GitBackend, error) {
	if repoURL == "" {
		return nil, fmt.Errorf("GIT_REPO_URL is required when using git storage backend. Please set GIT_REPO_URL environment variable.")
	}
	if branch == "" {
		branch = "main"
	}
	if accountsPath == "" {
		accountsPath = "accounts.json"
	}
	if authKeysPath == "" {
		authKeysPath = "auth_keys.json"
	}
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "chatgpt2api_git_cache")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	return &GitBackend{
		repoURL:      repoURL,
		token:        token,
		branch:       branch,
		accountsPath: accountsPath,
		authKeysPath: authKeysPath,
		cacheDir:     cacheDir,
	}, nil
}

func (b *GitBackend) auth() (transport.AuthMethod, error) {
	if b.token == "" {
		return nil, nil
	}
	return &http.BasicAuth{Username: "x-access-token", Password: b.token}, nil
}

// openOrClone 返回已 pull 到最新的仓库句柄。
func (b *GitBackend) openOrClone() (*git.Repository, error) {
	repoPath := filepath.Join(b.cacheDir, "repo")
	if info, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil && info.IsDir() {
		repo, err := git.PlainOpen(repoPath)
		if err == nil {
			auth, authErr := b.auth()
			if authErr != nil {
				return nil, authErr
			}
			worktree, err := repo.Worktree()
			if err == nil {
				pullErr := worktree.Pull(&git.PullOptions{
					RemoteName:    "origin",
					ReferenceName: plumbing.NewBranchReferenceName(b.branch),
					Auth:          auth,
					Force:         true,
				})
				if pullErr == nil || pullErr == git.NoErrAlreadyUpToDate {
					return repo, nil
				}
			}
		}
		// 拉取失败则删除重新克隆，与 Python 行为一致。
		_ = os.RemoveAll(repoPath)
	}
	auth, err := b.auth()
	if err != nil {
		return nil, err
	}
	return git.PlainClone(repoPath, false, &git.CloneOptions{
		URL:           b.repoURL,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(b.branch),
		SingleBranch:  true,
	})
}

func (b *GitBackend) readFile(relativePath string) ([]byte, error) {
	repo, err := b.openOrClone()
	if err != nil {
		return nil, err
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err
	}
	payload, err := os.ReadFile(filepath.Join(worktree.Filesystem.Root(), relativePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return payload, nil
}

func (b *GitBackend) writeFile(relativePath string, value any, message string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	repo, err := b.openOrClone()
	if err != nil {
		return err
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return err
	}
	fullPath := filepath.Join(worktree.Filesystem.Root(), relativePath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if err := os.WriteFile(fullPath, payload, 0o644); err != nil {
		return err
	}
	if _, err := worktree.Add(relativePath); err != nil {
		return err
	}
	status, err := worktree.Status()
	if err != nil {
		return err
	}
	if status.IsClean() {
		return nil
	}
	commitHash, err := worktree.Commit(message, &git.CommitOptions{})
	if err != nil {
		return err
	}
	_ = commitHash
	auth, err := b.auth()
	if err != nil {
		return err
	}
	return repo.Push(&git.PushOptions{
		RemoteName: "origin",
		Auth:       auth,
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", b.branch, b.branch)),
		},
	})
}

func (b *GitBackend) LoadAccounts() ([]map[string]any, error) {
	payload, err := b.readFile(b.accountsPath)
	if err != nil {
		return nil, err
	}
	if payload == nil {
		return []map[string]any{}, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(payload, &items); err != nil {
		return []map[string]any{}, nil
	}
	return items, nil
}

func (b *GitBackend) SaveAccounts(accounts []map[string]any) error {
	return b.writeFile(b.accountsPath, accounts, "Update accounts data")
}

func (b *GitBackend) LoadAuthKeys() ([]map[string]any, error) {
	payload, err := b.readFile(b.authKeysPath)
	if err != nil {
		return nil, err
	}
	if payload == nil {
		return []map[string]any{}, nil
	}
	var wrapper struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(payload, &wrapper); err == nil && wrapper.Items != nil {
		return wrapper.Items, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(payload, &items); err != nil {
		return []map[string]any{}, nil
	}
	return items, nil
}

func (b *GitBackend) SaveAuthKeys(authKeys []map[string]any) error {
	return b.writeFile(b.authKeysPath, map[string]any{"items": authKeys}, "Update auth keys data")
}

func (b *GitBackend) HealthCheck() map[string]any {
	repo, err := b.openOrClone()
	if err != nil {
		return map[string]any{"status": "unhealthy", "backend": "git", "error": err.Error()}
	}
	head, err := repo.Head()
	lastCommit := ""
	if err == nil {
		hash := head.Hash().String()
		if len(hash) >= 8 {
			lastCommit = hash[:8]
		} else {
			lastCommit = hash
		}
	}
	return map[string]any{
		"status":              "healthy",
		"backend":             "git",
		"repo_url":            maskToken(b.repoURL),
		"branch":              b.branch,
		"file_path":           b.accountsPath,
		"auth_keys_file_path": b.authKeysPath,
		"last_commit":         lastCommit,
	}
}

func (b *GitBackend) BackendInfo() map[string]any {
	return map[string]any{
		"type":                "git",
		"description":         "Git 私有仓库存储",
		"repo_url":            maskToken(b.repoURL),
		"branch":              b.branch,
		"file_path":           b.accountsPath,
		"auth_keys_file_path": b.authKeysPath,
	}
}

// maskToken 复刻 Python 的 _mask_token：保留 scheme 与 host，其余打码。
func maskToken(url string) string {
	scheme, rest, ok := splitScheme(url)
	if !ok {
		return url
	}
	at := indexByte(rest, '@')
	if at < 0 {
		return url
	}
	return fmt.Sprintf("%s://****@%s", scheme, rest[at+1:])
}
