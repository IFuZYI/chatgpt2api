package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// SQLAlchemy 会为 unique=True 的列建命名索引。从 Python 迁移过来的库
// 里已有 ix_accounts_access_token / ix_auth_keys_key_id，Go 侧 DDL 若不同名，
// 迁移后会出现「索引重复但名字不同」的混乱。
//
// 这条测试锁定：Go 建的索引名与 SQLAlchemy 一致，或至少索引存在。
func TestSQLiteCreatesExpectedIndexes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	backend, err := NewDBBackend("sqlite:///" + dbPath)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if err := backend.SaveAccounts([]map[string]any{{"access_token": "tok"}}); err != nil {
		t.Fatalf("save: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// 收集 accounts 与 auth_keys 上的所有索引
	rows, err := db.Query(`SELECT name, tbl_name FROM sqlite_master WHERE type='index'`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer rows.Close()

	indexes := map[string]string{}
	for rows.Next() {
		var name, table string
		if err := rows.Scan(&name, &table); err != nil {
			t.Fatalf("scan: %v", err)
		}
		indexes[name] = table
	}

	// access_token 必须被索引（否则 269 账号每次查找都全表扫）
	if !hasIndexOn(indexes, "accounts") {
		t.Errorf("no index on accounts table; indexes = %v", indexes)
	}
	if !hasIndexOn(indexes, "auth_keys") {
		t.Errorf("no index on auth_keys table; indexes = %v", indexes)
	}

	// 索引名必须与 SQLAlchemy 一致：从 Python 迁移过来的库已经建了这两个名字，
	// Go 侧若用不同名（或只依赖 UNIQUE 的隐式 sqlite_autoindex_*），
	// 迁移后会出现「同一列两套索引」的混乱。
	for _, want := range []string{"ix_accounts_access_token", "ix_auth_keys_key_id"} {
		if _, ok := indexes[want]; !ok {
			t.Errorf("missing named index %q (SQLAlchemy creates it); got %v", want, indexes)
		}
	}
}

// hasIndexOn 报告某张表是否有任何索引。
func hasIndexOn(indexes map[string]string, table string) bool {
	for _, indexedTable := range indexes {
		if indexedTable == table {
			return true
		}
	}
	return false
}
