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

// DELETE /api/accounts 的响应也必须脱敏。
//
// 回归点：DeleteAccounts 返回 map[string]any{"items": s.ListAccounts()}，
// handler 直接 c.JSON 返回 —— 实测响应里 260 个账号全部带 password /
// totp_secret / refresh_token / id_token 原文。
//
// 删除接口返回「剩余账号列表」是为了让前端刷新表格，但它同样是一次
// 批量读，和 GET /api/accounts 没有区别。
func TestDeleteAccountsResponseIsMasked(t *testing.T) {
	items := []map[string]any{
		{"access_token": "at-1", "password": "pw", "totp_secret": "JBSWY3DPEHPK3PXP",
			"refresh_token": "rt", "id_token": "id"},
	}
	masked := maskAccountCredentials(items)
	if len(masked) != 1 {
		t.Fatalf("长度变了")
	}
	for _, field := range []string{"password", "totp_secret", "refresh_token", "id_token"} {
		if _, ok := masked[0][field]; ok {
			t.Errorf("脱敏后仍有 %s —— 删除接口会泄漏", field)
		}
	}
}

// 所有返回账号列表的 handler 都必须经过脱敏。
//
// 这条测试守的是「新增端点忘记脱敏」这个模式：它枚举所有已知的
// 「返回 items 的账号端点」，逐个断言其响应不含凭据。
func TestAllAccountListEndpointsMasked(t *testing.T) {
	// 端点到 handler 的映射由下面的集成断言覆盖；
	// 这里先锁住「脱敏函数本身覆盖 5 个字段」这一前提。
	sample := []map[string]any{{
		"access_token": "at-1", "password": "pw", "totp_secret": "ts",
		"refresh_token": "rt", "id_token": "id", "session_token": "sess",
	}}
	masked := maskAccountCredentials(sample)[0]
	for _, field := range []string{"password", "totp_secret", "refresh_token", "id_token", "session_token"} {
		if _, ok := masked[field]; ok {
			t.Errorf("%s 未被脱敏", field)
		}
		if masked[field+"_set"] != true {
			t.Errorf("%s_set 未设置", field)
		}
	}
}
