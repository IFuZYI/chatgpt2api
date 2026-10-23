package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	_ "modernc.org/sqlite"             // pure-Go sqlite driver
)

// DBBackend 复刻 services/storage/database_storage.py。
//
// 表结构与 Python/SQLAlchemy 建出来的一致（已实测 DDL）：
//
//	CREATE TABLE accounts (id INTEGER PRIMARY KEY AUTOINCREMENT,
//	    access_token VARCHAR(2048) NOT NULL UNIQUE, data TEXT NOT NULL)
//	CREATE TABLE auth_keys (id INTEGER PRIMARY KEY AUTOINCREMENT,
//	    key_id VARCHAR(255) NOT NULL UNIQUE, data TEXT NOT NULL)
//
// 增量语义也照抄：新键插入、内容变化才更新、消失的键删除。
type DBBackend struct {
	db     *sql.DB
	url    string
	driver string
	mu     sync.Mutex
}

// NewDBBackend 接受与 Python 相同的 DATABASE_URL 形态：
// sqlite:///path、postgresql://...、postgres://...
func NewDBBackend(databaseURL string) (*DBBackend, error) {
	driver, dsn, err := parseDatabaseURL(databaseURL)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	b := &DBBackend{db: db, url: databaseURL, driver: driver}
	if err := b.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return b, nil
}

func parseDatabaseURL(databaseURL string) (driver, dsn string, err error) {
	trimmed := strings.TrimSpace(databaseURL)
	switch {
	case strings.HasPrefix(trimmed, "sqlite:///"):
		return "sqlite", strings.TrimPrefix(trimmed, "sqlite:///"), nil
	case strings.HasPrefix(trimmed, "sqlite://"):
		return "sqlite", strings.TrimPrefix(trimmed, "sqlite://"), nil
	case strings.HasPrefix(trimmed, "postgresql://"), strings.HasPrefix(trimmed, "postgres://"):
		return "pgx", trimmed, nil
	default:
		return "", "", fmt.Errorf("unsupported database url: %s", maskPassword(databaseURL))
	}
}

func (b *DBBackend) migrate() error {
	statements := []string{}
	if b.driver == "sqlite" {
		statements = []string{
			`CREATE TABLE IF NOT EXISTS accounts (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				access_token VARCHAR(2048) NOT NULL UNIQUE,
				data TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS auth_keys (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				key_id VARCHAR(255) NOT NULL UNIQUE,
				data TEXT NOT NULL)`,
		}
	} else {
		statements = []string{
			`CREATE TABLE IF NOT EXISTS accounts (
				id BIGSERIAL PRIMARY KEY,
				access_token VARCHAR(2048) NOT NULL UNIQUE,
				data TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS auth_keys (
				id BIGSERIAL PRIMARY KEY,
				key_id VARCHAR(255) NOT NULL UNIQUE,
				data TEXT NOT NULL)`,
		}
	}
	for _, statement := range statements {
		if _, err := b.db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (b *DBBackend) LoadAccounts() ([]map[string]any, error) {
	return b.loadRows("accounts", "access_token")
}

func (b *DBBackend) SaveAccounts(accounts []map[string]any) error {
	// accounts 的 item 字段与列名都是 access_token。
	return b.saveRows("accounts", "access_token", "access_token", accounts)
}

func (b *DBBackend) LoadAuthKeys() ([]map[string]any, error) {
	return b.loadRows("auth_keys", "key_id")
}

func (b *DBBackend) SaveAuthKeys(authKeys []map[string]any) error {
	// auth key 的 item 里字段叫 "id"，但数据库列叫 "key_id"。
	// 两者必须分开传：Python 的 _save_rows(model, items, "id", "key_id")。
	//
	// 早期实现只传 "key_id"，于是 item["key_id"] 恒为空 → 每个 key 都被
	// `continue` 跳过（插入 0 行），而删除循环仍清空整表 →
	// SQLite/Postgres 后端下所有鉴权密钥静默消失（实测：创建成功、
	// 列表为 0、鉴权 401）。这是数据丢失级缺陷。
	return b.saveRows("auth_keys", "id", "key_id", authKeys)
}

func (b *DBBackend) loadRows(table, keyColumn string) ([]map[string]any, error) {
	rows, err := b.db.Query(fmt.Sprintf("SELECT %s, data FROM %s ORDER BY id", keyColumn, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var key, payload string
		if err := rows.Scan(&key, &payload); err != nil {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(payload), &item); err != nil {
			continue
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// saveRows 复刻 Python 的 _save_rows：一轮比对后 insert/update/delete。
// saveRows 把 items 增量同步到表。
//
// sourceKey 是 item 里的字段名，targetKey 是数据库列名。
// 两者必须分开：auth_keys 的 item 用 "id"、列用 "key_id"。
// 合并成一个参数会让 auth_keys 的插入全部被跳过（见 SaveAuthKeys 注释）。
func (b *DBBackend) saveRows(table, sourceKey, targetKey string, items []map[string]any) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	existing := map[string]string{} // key -> data
	rows, err := tx.Query(fmt.Sprintf("SELECT %s, data FROM %s", targetKey, table))
	if err != nil {
		return err
	}
	for rows.Next() {
		var key, payload string
		if err := rows.Scan(&key, &payload); err != nil {
			rows.Close()
			return err
		}
		existing[key] = payload
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	incoming := make(map[string]bool, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		key, _ := item[sourceKey].(string)
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if incoming[key] {
			return ErrDuplicateKey
		}
		incoming[key] = true

		payload, err := json.Marshal(item)
		if err != nil {
			return err
		}
		serialized := string(payload)
		previous, ok := existing[key]
		if !ok {
			if _, err := tx.Exec(
				fmt.Sprintf("INSERT INTO %s (%s, data) VALUES (%s, %s)", table, targetKey, b.placeholder(1), b.placeholder(2)),
				key, serialized,
			); err != nil {
				return err
			}
			continue
		}
		if previous != serialized {
			if _, err := tx.Exec(
				fmt.Sprintf("UPDATE %s SET data = %s WHERE %s = %s", table, b.placeholder(1), targetKey, b.placeholder(2)),
				serialized, key,
			); err != nil {
				return err
			}
		}
	}

	for key := range existing {
		if incoming[key] {
			continue
		}
		if _, err := tx.Exec(
			fmt.Sprintf("DELETE FROM %s WHERE %s = %s", table, targetKey, b.placeholder(1)), key,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (b *DBBackend) placeholder(index int) string {
	if b.driver == "pgx" {
		return fmt.Sprintf("$%d", index)
	}
	return "?"
}

func (b *DBBackend) HealthCheck() map[string]any {
	var accountCount, authKeyCount int
	if err := b.db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&accountCount); err != nil {
		return map[string]any{"status": "unhealthy", "backend": "database", "error": err.Error()}
	}
	if err := b.db.QueryRow("SELECT COUNT(*) FROM auth_keys").Scan(&authKeyCount); err != nil {
		return map[string]any{"status": "unhealthy", "backend": "database", "error": err.Error()}
	}
	return map[string]any{
		"status":         "healthy",
		"backend":        "database",
		"database_url":   maskPassword(b.url),
		"account_count":  accountCount,
		"auth_key_count": authKeyCount,
	}
}

func (b *DBBackend) BackendInfo() map[string]any {
	dbType := "unknown"
	switch {
	case strings.Contains(b.url, "sqlite"):
		dbType = "sqlite"
	case strings.Contains(b.url, "postgres"):
		dbType = "postgresql"
	case strings.Contains(b.url, "mysql"):
		dbType = "mysql"
	}
	return map[string]any{
		"type":         "database",
		"db_type":      dbType,
		"description":  fmt.Sprintf("数据库存储 (%s)", dbType),
		"database_url": maskPassword(b.url),
	}
}

func (b *DBBackend) Close() error { return b.db.Close() }
