package api

import "testing"

// 异步图片任务必须用 url 格式，不能存 base64。
//
// 回归点：image_task_runner 硬编码 ResponseFormat: "b64_json"，每张图的
// base64（1.2-1.6MB）都写进 data/image_tasks.json，而 saveLocked 每次
// 进度回调都整文件重写。实测 9 个任务 = 31MB，其中 30.8MB 是 base64。
// Python 用 "url"（image_task_service.py:139,165），只存 URL。
func TestImageTaskUsesURLResponseFormat(t *testing.T) {
	if got := imageTaskResponseFormat(); got != "url" {
		t.Errorf("image task response_format = %q, want %q (b64_json makes image_tasks.json grow to MBs)",
			got, "url")
	}
}
