package storage

import "errors"

// ErrDuplicateKey 对应 Python 的 `ValueError(f"Duplicate {source_key} in storage snapshot")`。
// 调用方按 500/400 处理时用它做判别。
var ErrDuplicateKey = errors.New("duplicate key in storage snapshot")

// Backend 是号池与鉴权密钥的唯一持久化收口。
// 四个实现：json / sqlite+postgres / git。接口刻意保持与 Python 版一一对应，
// 因为上层 account_service 与 auth_service 的写盘时机已经由它决定了。
type Backend interface {
	// LoadAccounts 返回全部账号对象，顺序为存储顺序。
	LoadAccounts() ([]map[string]any, error)
	// SaveAccounts 覆盖式保存；数据库与 git 实现内部做增量。
	SaveAccounts(accounts []map[string]any) error

	LoadAuthKeys() ([]map[string]any, error)
	SaveAuthKeys(authKeys []map[string]any) error

	HealthCheck() map[string]any
	BackendInfo() map[string]any
}
