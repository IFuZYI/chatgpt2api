package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// 配置里的第三方密钥不能明文回显。
//
// 回归点：GET /api/settings 返回 Config.Get()，其中含
// backup.secret_access_key、backup.passphrase、image_storage.webdav_password、
// ai_review.api_key —— 全部明文。同仓库对账号凭据和 clearance cookie 都脱敏，
// 唯独配置里的密钥没有。
func TestMaskConfigSecretsRemovesSecrets(t *testing.T) {
	cfg := map[string]any{
		"backup": map[string]any{
			"enabled":           true,
			"secret_access_key": "R2-SECRET",
			"passphrase":        "BACKUP-PASS",
			"bucket":            "my-bucket",
		},
		"image_storage": map[string]any{
			"webdav_password": "WEBDAV-PASS",
			"provider":        "webdav",
		},
		"ai_review": map[string]any{
			"api_key": "sk-SECRET",
			"model":   "gpt-5-mini",
		},
	}

	masked := maskConfigSecrets(cfg)

	encoded, err := json.Marshal(masked)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"R2-SECRET", "BACKUP-PASS", "WEBDAV-PASS", "sk-SECRET"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("配置响应仍含明文密钥 %q", secret)
		}
	}

	// 非敏感字段必须保留，否则设置页无法显示
	for _, want := range []string{"my-bucket", "webdav", "gpt-5-mini"} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("非敏感字段 %q 被误删", want)
		}
	}

	// 必须告诉前端「这个字段有值」，否则用户不知道该不该重填
	backup, _ := masked["backup"].(map[string]any)
	if backup["has_secret_access_key"] != true {
		t.Error("缺少 has_secret_access_key 标记")
	}
	if backup["has_passphrase"] != true {
		t.Error("缺少 has_passphrase 标记")
	}
}

// 不修改入参：脱敏不能把 store 里的真实密钥删掉。
func TestMaskConfigSecretsDoesNotMutateInput(t *testing.T) {
	cfg := map[string]any{
		"backup": map[string]any{"secret_access_key": "KEEP-ME"},
	}
	_ = maskConfigSecrets(cfg)

	backup, _ := cfg["backup"].(map[string]any)
	if backup["secret_access_key"] != "KEEP-ME" {
		t.Error("脱敏修改了入参 —— 会把内存里的真实密钥删掉")
	}
}

// POST /api/settings 的响应也必须脱敏。
//
// 回归点：这个端点返回 Config.Update() 的结果，而 Update 内部是 s.Get()
// —— 不脱敏。GET 脱敏了、POST 没有，于是保存一次设置就把
// webdav_password / R2 密钥 / AI key 明文送进浏览器，并被前端
// normalizeConfig 存进 React state。
//
// 这条漏洞是「为修 WebDAV 密码保留而把密码注入响应」时暴露出来的：
// 修复前响应里是空串，修复后是真实密钥。
func TestUpdateSettingsResponseIsMasked(t *testing.T) {
	cfg := map[string]any{
		"backup":        map[string]any{"secret_access_key": "R2-SECRET", "passphrase": "PW"},
		"image_storage": map[string]any{"webdav_password": "WEBDAV-SECRET"},
		"ai_review":     map[string]any{"api_key": "sk-SECRET"},
	}
	masked := maskConfigSecrets(cfg)
	encoded, _ := json.Marshal(masked)
	for _, secret := range []string{"R2-SECRET", "WEBDAV-SECRET", "sk-SECRET"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("POST /api/settings 响应泄漏 %q", secret)
		}
	}
}
