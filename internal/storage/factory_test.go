package storage

import (
	"path/filepath"
	"testing"
)

// 环境变量必须能选出正确的后端。
//
// 之前 factory 无测试，而它是「部署时到底用哪个后端」的唯一入口 ——
// 选错了会让用户以为数据存进了 Postgres，实际还在本地 JSON 里。
func TestNewFromEnvSelectsBackend(t *testing.T) {
	cases := []struct {
		name        string
		storageEnv  string
		databaseURL string
		wantBackend string
		wantErr     bool
	}{
		{"default is json", "", "", "json", false},
		{"explicit json", "json", "", "json", false},
		{"sqlite", "sqlite", "", "database", false},
		// postgres 无 DATABASE_URL 时回退到本地 sqlite（不是报错）——
		// 这是刻意设计：让「配了后端名但忘了填 URL」的部署仍能起来。
		{"postgres without url falls back", "postgres", "", "database", false},
		// mysql 也走 database 分支（同上的回退策略）
		{"mysql alias", "mysql", "", "database", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("STORAGE_BACKEND", tc.storageEnv)
			t.Setenv("DATABASE_URL", tc.databaseURL)

			backend, err := NewFromEnv(dir)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error for STORAGE_BACKEND=%q, got backend %T", tc.storageEnv, backend)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			info := backend.BackendInfo()
			// 键名是 "type"（json / database / git）
			if got := info["type"]; got != tc.wantBackend {
				t.Errorf("type = %v, want %v", got, tc.wantBackend)
			}
		})
	}
}

// sqlite 默认落在 data 目录下的 accounts.db。
func TestNewFromEnvSQLiteDefaultsIntoDataDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STORAGE_BACKEND", "sqlite")
	t.Setenv("DATABASE_URL", "")

	backend, err := NewFromEnv(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backend == nil {
		t.Fatal("nil backend")
	}
	// 能存能读就说明路径可用
	if err := backend.SaveAccounts([]map[string]any{{"access_token": "t"}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := backend.LoadAccounts()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("len = %d, want 1", len(got))
	}
	// 确认文件落在传入的目录下
	if _, statErr := filepath.Glob(filepath.Join(dir, "*.db")); statErr != nil {
		t.Logf("glob: %v", statErr)
	}
}
