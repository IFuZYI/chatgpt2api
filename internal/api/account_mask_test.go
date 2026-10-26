package api

import (
	"testing"
)

// 所有返回账号列表的端点都必须脱敏。
//
// 回归点：脱敏函数 maskAccountCredentials 只在 GET /api/accounts 里调用，
// 而 /api/accounts/update 与 /api/accounts/proxy 直接返回
// d.Accts.ListAccounts() —— 实测这两个端点会把 password 与 totp_secret
// 的**原文**返回给调用方。虽然它们都要 admin 权限，但：
//  1. 列表接口刻意脱敏说明设计意图就是「不在批量读里给原文」
//  2. 前端 update 之后会把 items 存进状态，等于凭据进了浏览器内存
//  3. 任何日志/代理/浏览器插件都能截到
func TestMaskAccountCredentialsRemovesSecrets(t *testing.T) {
	items := []map[string]any{
		{
			"access_token":  "at-1",
			"password":      "plain-pw",
			"totp_secret":   "JBSWY3DPEHPK3PXP",
			"refresh_token": "rt-plain",
			"id_token":      "id-plain",
			"session_token": "sess-plain",
		},
	}
	masked := maskAccountCredentials(items)

	if len(masked) != 1 {
		t.Fatalf("长度变了: %d", len(masked))
	}
	for _, field := range []string{"password", "totp_secret", "refresh_token", "id_token", "session_token"} {
		if value, ok := masked[0][field]; ok {
			t.Errorf("%s 仍存在（值 %v）—— 应被替换成 %s_set", field, value, field)
		}
		if masked[0][field+"_set"] != true {
			t.Errorf("%s_set 应为 true", field)
		}
	}
	// access_token 必须保留：前端用它做行标识
	if masked[0]["access_token"] != "at-1" {
		t.Errorf("access_token 被误删: %v", masked[0]["access_token"])
	}
}

// 脱敏不能污染原切片（号池内存里的账号必须保留凭据）。
func TestMaskAccountCredentialsDoesNotMutateInput(t *testing.T) {
	original := map[string]any{"access_token": "at-1", "password": "plain-pw"}
	items := []map[string]any{original}
	_ = maskAccountCredentials(items)

	if original["password"] != "plain-pw" {
		t.Error("脱敏改写了原 map —— 号池内存里的密码被删了，后续重登会失败")
	}
}
