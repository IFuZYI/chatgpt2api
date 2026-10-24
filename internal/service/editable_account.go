package service

import (
	"errors"
	"sort"
	"strings"
)

// 可编辑文件（PPT/PSD）链路的选号。
//
// 复刻 services/editable_file_task_service.py:55-66 的 _editable_access_token。
// 与图片链路的差别：
//   - 不看额度（quota）：可编辑文件不消耗 image_gen 额度
//   - 只取 Plus/Team/Pro/Enterprise：免费号跑不了 Agent 沙箱
//   - 按 last_used_at 升序（最久没用的先用），不是轮询游标

// ErrNoEditableAccount 与 Python 的 "no available plus/team/pro account" 对应。
var ErrNoEditableAccount = errors.New("no available plus/team/pro account")

// EditableFileAccessToken 选一个可用于可编辑文件导出的账号 token。
//
// 排序用 last_used_at 的字符串比较：Python 的 sort(key=...) 对 None/"" 的处理
// 是把空值排最前（最久没用），Go 侧对齐这一点 —— 空串字典序最小。
func (s *AccountService) EditableFileAccessToken() (string, error) {
	s.mu.RLock()
	candidates := make([]Account, 0, len(s.accounts))
	for _, account := range s.accounts {
		if strings.TrimSpace(stringValue(account["access_token"])) == "" {
			continue
		}
		switch stringValue(account["status"]) {
		case StatusDisabled, StatusAbnormal:
			continue
		}
		if !accountMatchesAnyPlanType(account, EditableFilePlanTypes) {
			continue
		}
		candidates = append(candidates, account)
	}
	s.mu.RUnlock()

	if len(candidates) == 0 {
		return "", ErrNoEditableAccount
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left := strings.TrimSpace(stringValue(candidates[i]["last_used_at"]))
		right := strings.TrimSpace(stringValue(candidates[j]["last_used_at"]))
		if left != right {
			return left < right
		}
		return stringValue(candidates[i]["access_token"]) < stringValue(candidates[j]["access_token"])
	})
	return strings.TrimSpace(stringValue(candidates[0]["access_token"])), nil
}

// EditableFileAccountEmail 返回 token 对应账号的邮箱（日志用，取不到给空串）。
func (s *AccountService) EditableFileAccountEmail(token string) string {
	account := s.GetAccount(token)
	if account == nil {
		return ""
	}
	return strings.TrimSpace(stringValue(account["email"]))
}
