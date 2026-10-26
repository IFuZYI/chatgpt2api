package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 入站请求体必须有上限。
//
// 回归点：decodeJSONBody 用 c.GetRawData() 无限制读入，全仓库
// MaxBytesReader 使用为 0。实测 1.5 MB 的 /v1/images/edits 请求体让
// RSS 从 1.8 MB 涨到 461 MB（+459 MB）、耗时 52 秒 —— 而它最终返回 429
// （号池空），即**图片解码发生在配额检查之前**，也不受并发槽位约束。
// 几个并发请求即可把单实例打 OOM。
func TestDecodeJSONBodyRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 上限是 8 MiB，用 12 MiB 确保超限。
	huge := `{"prompt":"` + strings.Repeat("A", 12<<20) + `"}`

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader([]byte(huge)))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(huge))
	c.Request = req

	_, httpErr := decodeJSONBody(c)
	if httpErr == nil {
		t.Fatal("12 MiB 请求体被接受 —— 上限未生效")
	}
	if httpErr.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 %d，应为 413", httpErr.Status)
	}
	t.Logf("12 MiB 请求体被拒: %d %s", httpErr.Status, httpErr.Error())
}

// 正常大小的请求体不能受影响。
func TestDecodeJSONBodyAcceptsNormalBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := `{"image_retention_days":7,"auth-key":"test"}`

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(payload))
	c.Request = req

	body, httpErr := decodeJSONBody(c)
	if httpErr != nil {
		t.Fatalf("正常请求体被拒: %d %s", httpErr.Status, httpErr.Error())
	}
	if body["image_retention_days"] != float64(7) {
		t.Errorf("解析结果错误: %v", body["image_retention_days"])
	}
}

// 号池导入不能被上限误伤。
//
// 回归点：控制面上限最初取 1 MiB，但实测 260 个账号的导出文件已有 880 KB
// （约 3.5 KB/账号）—— 1 MiB 只能装 302 个账号，一个 300 号以上的池子
// 导出后**无法再导入**。这条测试钉住「至少能装 2000 个账号」。
func TestControlPlaneLimitFitsLargeAccountImport(t *testing.T) {
	const accounts = 2000
	const perAccount = 3500 // 实测约 3.5 KB/账号
	need := int64(accounts * perAccount)

	if int64(maxJSONBodyBytes) < need {
		t.Fatalf("控制面上限 %d B 装不下 %d 个账号的导入包（需约 %d B）",
			maxJSONBodyBytes, accounts, need)
	}
	t.Logf("控制面上限 %d B 可容纳约 %d 个账号",
		maxJSONBodyBytes, int64(maxJSONBodyBytes)/perAccount)
}

// 图片端点的上限必须大于控制面：base64 有 4/3 膨胀。
func TestImageLimitExceedsControlPlane(t *testing.T) {
	if maxImageBodyBytes <= maxJSONBodyBytes {
		t.Fatalf("图片上限 %d 应大于控制面上限 %d", maxImageBodyBytes, maxJSONBodyBytes)
	}
}
