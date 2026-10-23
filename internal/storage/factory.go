package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Factory 复刻 services/storage/factory.py 的环境变量契约：
//
//	STORAGE_BACKEND = json | sqlite | postgres | postgresql | mysql | database | git
//	DATABASE_URL    = sqlite:///... | postgresql://...
//	GIT_REPO_URL / GIT_TOKEN / GIT_BRANCH / GIT_FILE_PATH / GIT_AUTH_KEYS_FILE_PATH
func NewFromEnv(dataDir string) (Backend, error) {
	backendType := strings.ToLower(strings.TrimSpace(os.Getenv("STORAGE_BACKEND")))
	if backendType == "" {
		backendType = "json"
	}
	fmt.Printf("[storage] Initializing storage backend: %s\n", backendType)

	switch backendType {
	case "json":
		accountsPath := filepath.Join(dataDir, "accounts.json")
		authKeysPath := filepath.Join(dataDir, "auth_keys.json")
		fmt.Printf("[storage] Using JSON storage: %s\n", accountsPath)
		return NewJSONBackend(accountsPath, authKeysPath)

	case "sqlite", "postgres", "postgresql", "mysql", "database":
		databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
		if databaseURL == "" {
			databaseURL = "sqlite:///" + filepath.Join(dataDir, "accounts.db")
			fmt.Printf("[storage] No DATABASE_URL provided, using local SQLite: %s\n", databaseURL)
		} else {
			fmt.Printf("[storage] Using database storage: %s\n", maskPassword(databaseURL))
		}
		return NewDBBackend(databaseURL)

	case "git":
		repoURL := strings.TrimSpace(os.Getenv("GIT_REPO_URL"))
		token := strings.TrimSpace(os.Getenv("GIT_TOKEN"))
		branch := strings.TrimSpace(os.Getenv("GIT_BRANCH"))
		if branch == "" {
			branch = "main"
		}
		accountsPath := strings.TrimSpace(os.Getenv("GIT_FILE_PATH"))
		if accountsPath == "" {
			accountsPath = "accounts.json"
		}
		authKeysPath := strings.TrimSpace(os.Getenv("GIT_AUTH_KEYS_FILE_PATH"))
		if authKeysPath == "" {
			authKeysPath = "auth_keys.json"
		}
		fmt.Printf("[storage] Using Git storage: %s, branch: %s, file: %s\n",
			maskToken(repoURL), branch, accountsPath)
		return NewGitBackend(
			repoURL,
			token,
			branch,
			accountsPath,
			authKeysPath,
			filepath.Join(dataDir, "git_cache"),
		)

	default:
		return nil, fmt.Errorf(
			"Unknown storage backend: %s. Supported backends: json, sqlite, postgres, git", backendType)
	}
}
