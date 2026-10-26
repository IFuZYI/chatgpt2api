package storage

import (
	"path/filepath"
	"testing"
)

// account_id 必须被 SQL 后端持久化，重开数据库后原样返回。
//
// 为什么值得一条专门的测试：JSON 后端把它当普通字段存进数组，
// 而 SQL 后端走「data 列存整个对象」的路径 —— 若序列化/反序列化
// 有问题（例如某处白名单过滤字段），account_id 会静默丢失，
// 导致每次重启都重新派生。
func TestAccountIDPersistsAcrossSQLReopen(t *testing.T) {
	dir := t.TempDir()
	url := "sqlite:///" + filepath.Join(dir, "t.db")

	be, err := NewDBBackend(url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := be.SaveAccounts([]map[string]any{
		{"access_token": "at-1", "account_id": "id-AAA", "email": "a@x.test"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	be.Close()

	reopened, err := NewDBBackend(url)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	rows, err := reopened.LoadAccounts()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("行数 %d，应为 1", len(rows))
	}
	if got := rows[0]["account_id"]; got != "id-AAA" {
		t.Errorf("account_id = %v，应为 id-AAA（丢失会让每次重启重新派生）", got)
	}
}

// 首次升级（老数据无 account_id）时，保存带 id 的新数据必须走 UPDATE
// 而非 DELETE+INSERT —— 否则自增 id 会变，外部按 id 的引用就断了。
func TestUpgradeKeepsAutoIncrementID(t *testing.T) {
	dir := t.TempDir()
	url := "sqlite:///" + filepath.Join(dir, "t.db")
	be, err := NewDBBackend(url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer be.Close()

	if err := be.SaveAccounts([]map[string]any{
		{"access_token": "at-1", "email": "a@x.test"},
		{"access_token": "at-2", "email": "b@x.test"},
	}); err != nil {
		t.Fatalf("save old: %v", err)
	}

	snapshot := func() map[string]int64 {
		rows, queryErr := be.db.Query(`SELECT id, access_token FROM accounts`)
		if queryErr != nil {
			t.Fatalf("query: %v", queryErr)
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var id int64
			var token string
			_ = rows.Scan(&id, &token)
			out[token] = id
		}
		return out
	}
	before := snapshot()

	if err := be.SaveAccounts([]map[string]any{
		{"access_token": "at-1", "email": "a@x.test", "account_id": "id-1"},
		{"access_token": "at-2", "email": "b@x.test", "account_id": "id-2"},
	}); err != nil {
		t.Fatalf("save upgraded: %v", err)
	}
	after := snapshot()

	for token, id := range before {
		if after[token] != id {
			t.Errorf("%s 的自增 id 从 %d 变成 %d —— 升级导致引用断裂", token, id, after[token])
		}
	}
}
