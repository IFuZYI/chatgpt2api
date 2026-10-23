package service

// NewBackupConfigError 构造配置类备份错误（文案直接对外展示）。
func NewBackupConfigError(message string) error {
	return &BackupError{Message: message}
}
