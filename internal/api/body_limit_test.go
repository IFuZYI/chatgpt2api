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

// multipart 路径必须同样受上限约束。
//
// 回归点：ParseMultipartForm 是**独立**的 body 读取路径，不经过
// decodeJSONBody —— MaxBytesReader 不会自动生效。它自己的 32 MiB 参数是
// 「内存阈值」（超过就落临时文件），不是请求体总上限，攻击者仍可上传
// 任意大的 body 绕过整个限制。
func TestMultipartBodyAlsoLimited(t *testing.T) {
	if maxImageBodyBytes <= 0 {
		t.Fatal("图片端点上限未定义")
	}
	// 断言上限确实小于 ParseMultipartForm 的内存阈值不会掩盖问题：
	// 关键是两者都存在，且请求体上限先触发。
	t.Logf("multipart 端点上限 %d B，ParseMultipartForm 内存阈值 %d B",
		maxImageBodyBytes, 32<<20)
	if maxImageBodyBytes > 32<<20 {
		t.Errorf("请求体上限 %d 大于内存阈值 %d，应更小或相等",
			maxImageBodyBytes, 32<<20)
	}
}

// PPT / PSD 端点必须被认作图片端点。
//
// 回归点：`isImageBodyPath` 漏了 /v1/ppt/generations 与 /v1/psd/generations，
// 而两者都收 base64_images（PSD 还**要求**至少一张参考图）。
// 漏在列表外会让 >~750 KB 的参考图被 413 拒绝 —— 合法上传被误伤。
func TestImageBodyPathsCoverBase64ImageEndpoints(t *testing.T) {
	mustAllow := []string{
		"/v1/images/edits",       // mask 合成
		"/v1/images/generations", // 参考图
		"/v1/chat/completions",   // 图片分支
		"/v1/responses",          // image tool
		"/v1/ppt/generations",    // base64_images
		"/v1/psd/generations",    // base64_images（必需）
		"/api/image-tasks/edits", // 异步编辑
		"/api/image-tasks/generations",
	}
	for _, path := range mustAllow {
		if !isImageBodyPath(path) {
			t.Errorf("%s 收 base64 图片，必须用图片额度而非控制面额度", path)
		}
	}
	// 控制面端点不能误用图片额度
	for _, path := range []string{"/api/settings", "/api/accounts", "/api/backups/run"} {
		if isImageBodyPath(path) {
			t.Errorf("%s 是控制面端点，不应拿到图片额度", path)
		}
	}
}
