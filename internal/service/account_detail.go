package service

import (
	"strings"
	"time"
)

// Detail 返回单个账号的完整详情，供管理面的「查看」弹窗使用。
//
// 与 ListAccounts 的差异是刻意的：
//   - ListAccounts 走 maskAccountCredentials，password / totp_secret 被删成
//     *_set 布尔值 —— 列表是批量读，一次泄漏等于全池接管。
//   - Detail 是显式的单条查询，必须给出凭据原文，否则「查看账号」这个功能
//     本身没有意义（用户要看的就是密码和 2FA 种子）。
//
// 凭据原文只在这里出现一次，调用方（handler）负责鉴权与审计。
//
// 派生字段（不改磁盘、只用于展示）：
//
//	access_token_expires_in  int     剩余秒数；-1 表示该 token 不是 JWT、无法判断
//	access_token_expires_at  string  RFC3339 绝对时间；未知时为空串
//	proxy_source             string  account | global | direct
//	proxy_effective          string  实际生效的代理地址（direct 时为空）
//	has_password / has_totp_secret / has_refresh_token / has_id_token / has_session_token
//	                                bool，供前端渲染「已配置 / 未配置」
//
// 返回 nil 表示账号不存在（含空 token）。返回值是副本，调用方改写它不会污染号池。
func (s *AccountService) Detail(token string) map[string]any {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return nil
	}

	s.mu.RLock()
	resolved := s.resolveTokenLocked(trimmed)
	account := s.accounts[resolved]
	if account == nil {
		s.mu.RUnlock()
		return nil
	}
	out := copyMap(account)
	s.mu.RUnlock()

	// AT 有效期：exp 缺失（非 JWT）时用 -1 表示「未知」。
	// 不能用 0：0 与「已过期」混淆，前端会显示「已过期」误导运维。
	expiresIn := -1
	expiresAt := ""
	if remaining := tokenExpiresIn(resolved); remaining != nil {
		expiresIn = *remaining
		expiresAt = time.Unix(jwtExp(resolved), 0).UTC().Format(time.RFC3339)
	}
	out["access_token_expires_in"] = expiresIn
	out["access_token_expires_at"] = expiresAt

	// 代理来源：必须说清楚「账号自带」还是「回退全局」，
	// 否则运维看到地址无法判断这个号是否真的走它。
	global := ""
	if s.cfg != nil {
		global = strings.TrimSpace(s.cfg.Proxy())
	}
	own := strings.TrimSpace(stringValue(account["proxy"]))
	switch {
	case own != "":
		out["proxy_source"] = "account"
		out["proxy_effective"] = own
	case global != "":
		out["proxy_source"] = "global"
		out["proxy_effective"] = global
	default:
		out["proxy_source"] = "direct"
		out["proxy_effective"] = ""
	}

	for _, field := range []string{"password", "totp_secret", "refresh_token", "id_token", "session_token"} {
		out["has_"+field] = strings.TrimSpace(stringValue(account[field])) != ""
	}

	return out
}
