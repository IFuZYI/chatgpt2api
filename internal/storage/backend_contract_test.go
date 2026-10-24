package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// backendFactory 是「能构造出可测后端」的工厂。
type backendFactory struct {
	name  string
	build func(t *testing.T) Backend
}

// backendFactories 列出所有可测后端。
//
// postgres 需要真实数据库，用环境变量开关（TEST_POSTGRES_URL），
// 否则跳过 —— 否则本地 `go test ./...` 会因为连不上而红。
func backendFactories(t *testing.T) []backendFactory {
	t.Helper()
	factories := []backendFactory{
		{"json", func(t *testing.T) Backend {
			dir := t.TempDir()
			backend, err := NewJSONBackend(
				filepath.Join(dir, "accounts.json"),
				filepath.Join(dir, "auth_keys.json"),
			)
			if err != nil {
				t.Fatalf("new json backend: %v", err)
			}
			return backend
		}},
		{"sqlite", func(t *testing.T) Backend {
			backend, err := NewDBBackend("sqlite:///" + filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatalf("new sqlite backend: %v", err)
			}
			return backend
		}},
	}
	if url := postgresTestURL(); url != "" {
		factories = append(factories, backendFactory{"postgres", func(t *testing.T) Backend {
			backend, err := NewDBBackend(url)
			if err != nil {
				t.Fatalf("new postgres backend: %v", err)
			}
			return backend
		}})
	}
	return factories
}

// 每个后端都必须满足同一套契约。
//
// 这组测试存在的原因：storage 包之前只有 4.9% 覆盖率，四个后端几乎无测试，
// 而「SQLite 保存密钥时清空整表」那个 Critical bug 就出在这里 ——
// saveRows 用 key_id 当源键，而 auth-key 条目用的是 id。
func TestBackendContract(t *testing.T) {
	for _, factory := range backendFactories(t) {
		factory := factory
		t.Run(factory.name, func(t *testing.T) {
			t.Run("accounts round-trip", func(t *testing.T) {
				backend := factory.build(t)
				want := []map[string]any{
					{"access_token": "tok-a", "email": "a@example.com", "quota": 25},
					{"access_token": "tok-b", "email": "b@example.com", "quota": 0},
				}
				if err := backend.SaveAccounts(want); err != nil {
					t.Fatalf("save: %v", err)
				}
				got, err := backend.LoadAccounts()
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				if len(got) != len(want) {
					t.Fatalf("len = %d, want %d", len(got), len(want))
				}
				byToken := map[string]map[string]any{}
				for _, item := range got {
					byToken[fmt.Sprint(item["access_token"])] = item
				}
				for _, expected := range want {
					token := fmt.Sprint(expected["access_token"])
					actual, ok := byToken[token]
					if !ok {
						t.Fatalf("account %q missing after round-trip", token)
					}
					if fmt.Sprint(actual["email"]) != fmt.Sprint(expected["email"]) {
						t.Errorf("%s email = %v, want %v", token, actual["email"], expected["email"])
					}
				}
			})

			// 这是「密钥被清空」那个 Critical bug 的回归点。
			t.Run("auth keys round-trip", func(t *testing.T) {
				backend := factory.build(t)
				want := []map[string]any{
					{"id": "key-1", "key_hash": "hash-1", "name": "first", "role": "user"},
					{"id": "key-2", "key_hash": "hash-2", "name": "second", "role": "admin"},
				}
				if err := backend.SaveAuthKeys(want); err != nil {
					t.Fatalf("save: %v", err)
				}
				got, err := backend.LoadAuthKeys()
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				if len(got) != len(want) {
					t.Fatalf("len = %d, want %d (keys were wiped)", len(got), len(want))
				}
				hashes := map[string]bool{}
				for _, item := range got {
					hashes[fmt.Sprint(item["key_hash"])] = true
				}
				for _, expected := range want {
					if !hashes[fmt.Sprint(expected["key_hash"])] {
						t.Errorf("key %q lost its key_hash", expected["id"])
					}
				}
			})

			t.Run("empty save clears", func(t *testing.T) {
				backend := factory.build(t)
				if err := backend.SaveAccounts([]map[string]any{{"access_token": "tok-x"}}); err != nil {
					t.Fatalf("seed: %v", err)
				}
				if err := backend.SaveAccounts(nil); err != nil {
					t.Fatalf("clear: %v", err)
				}
				got, err := backend.LoadAccounts()
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				if len(got) != 0 {
					t.Errorf("len = %d after clearing, want 0", len(got))
				}
			})

			t.Run("incremental delete", func(t *testing.T) {
				backend := factory.build(t)
				if err := backend.SaveAccounts([]map[string]any{
					{"access_token": "keep", "email": "k@example.com"},
					{"access_token": "drop", "email": "d@example.com"},
				}); err != nil {
					t.Fatalf("seed: %v", err)
				}
				// 只留 keep：drop 必须被真的删掉（增量同步的删除分支）
				if err := backend.SaveAccounts([]map[string]any{
					{"access_token": "keep", "email": "k@example.com"},
				}); err != nil {
					t.Fatalf("resave: %v", err)
				}
				got, err := backend.LoadAccounts()
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				if len(got) != 1 || fmt.Sprint(got[0]["access_token"]) != "keep" {
					t.Errorf("after incremental delete got %v, want only keep", got)
				}
			})

			t.Run("update existing", func(t *testing.T) {
				backend := factory.build(t)
				if err := backend.SaveAccounts([]map[string]any{
					{"access_token": "tok", "email": "old@example.com", "quota": 1},
				}); err != nil {
					t.Fatalf("seed: %v", err)
				}
				if err := backend.SaveAccounts([]map[string]any{
					{"access_token": "tok", "email": "new@example.com", "quota": 9},
				}); err != nil {
					t.Fatalf("update: %v", err)
				}
				got, err := backend.LoadAccounts()
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				if len(got) != 1 {
					t.Fatalf("len = %d, want 1 (update must not duplicate)", len(got))
				}
				if fmt.Sprint(got[0]["email"]) != "new@example.com" {
					t.Errorf("email = %v, want new@example.com", got[0]["email"])
				}
			})

			t.Run("health and info", func(t *testing.T) {
				backend := factory.build(t)
				health := backend.HealthCheck()
				if health == nil {
					t.Error("HealthCheck returned nil")
				}
				info := backend.BackendInfo()
				if info == nil {
					t.Error("BackendInfo returned nil")
				}
			})
		})
	}
}

// postgresTestURL 返回测试用 Postgres 连接串（未设置则跳过该后端）。
func postgresTestURL() string {
	return strings.TrimSpace(os.Getenv("TEST_POSTGRES_URL"))
}
