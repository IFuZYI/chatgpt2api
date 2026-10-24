package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// both 模式下远端失败时，本地已存的图必须仍然可用。
//
// 回归点：SaveImageBytes 在 both 模式下先写本地、再 PUT 远端，
// 远端失败时直接 return err —— 本地文件成了孤儿（存在但没进索引，
// 用户拿不到 URL），整个生图请求变 502。
//
// 这在工程上不合理：本地已经有副本，为什么因为远端抖动让用户拿不到图？
// 但也不能假装成功 —— 索引必须如实标 storage=local / webdav=false，
// 让调用方看得出这张图没上远端（SyncToWebDAV 可事后补传）。
//
// 注意与 webdav 模式的区别：webdav 模式本地没有副本，失败必须报错
// （那是另一个测试 TestSaveImageBytesWebDAVFailureFailsSave 覆盖的）。
func TestSaveImageBytesBothModeDegradesWhenWebDAVFails(t *testing.T) {
	recorder := newWebDAVRecorder()
	recorder.putStatus = http.StatusUnauthorized
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, cfg := newImageStorageFixture(t, "both", server.URL)

	rel, url, err := svc.SaveImageBytes(testPNG(t, 2, 2), "http://host")
	if err != nil {
		t.Fatalf("both mode must degrade to local when WebDAV fails, got error: %v", err)
	}
	if rel == "" || url == "" {
		t.Fatalf("rel/url must be non-empty (rel=%q url=%q)", rel, url)
	}

	// 本地文件必须真的在
	files := localImageFiles(t, cfg)
	if len(files) != 1 {
		t.Fatalf("local file missing after degraded save: %v", files)
	}

	// 索引必须如实标记：storage=local，webdav=false
	items := indexAll(t, cfg)
	item, ok := items[rel]
	if !ok {
		t.Fatalf("index entry missing for %q (items=%v)", rel, items)
	}
	if got := stringValue(item["storage"]); got != "local" {
		t.Errorf("storage = %q, want %q (must not claim 'both' when remote failed)", got, "local")
	}
	if truthy(item["webdav"]) {
		t.Error("webdav = true, want false (remote PUT failed)")
	}
	if !truthy(item["local"]) {
		t.Error("local = true, want true")
	}
	if got := stringValue(item["remote_url"]); got != "" {
		t.Errorf("remote_url = %q, want empty", got)
	}
}

// 降级后的图必须能通过本服务读到（走本地路径）。
func TestDegradedBothModeImageIsReadable(t *testing.T) {
	recorder := newWebDAVRecorder()
	recorder.putStatus = http.StatusInternalServerError
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, _ := newImageStorageFixture(t, "both", server.URL)

	rel, _, err := svc.SaveImageBytes(testPNG(t, 2, 2), "http://host")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	payload, mime, readErr := svc.ReadImage(rel)
	if readErr != nil {
		t.Fatalf("degraded image unreadable: %v", readErr)
	}
	if len(payload) == 0 {
		t.Error("empty payload")
	}
	if !strings.Contains(mime, "image/png") {
		t.Errorf("mime = %q, want image/png", mime)
	}
}

// webdav 模式（本地无副本）失败时仍必须报错 —— 不能静默吞掉。
func TestWebDAVOnlyModeStillFailsOnRemoteError(t *testing.T) {
	recorder := newWebDAVRecorder()
	recorder.putStatus = http.StatusUnauthorized
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, cfg := newImageStorageFixture(t, "webdav", server.URL)

	if _, _, err := svc.SaveImageBytes(testPNG(t, 2, 2), "http://host"); err == nil {
		t.Fatal("webdav-only mode must fail when remote PUT fails (no local copy exists)")
	}
	// 也不能偷偷留个本地文件
	if files := localImageFiles(t, cfg); len(files) != 0 {
		t.Errorf("webdav mode must not write local files, got %v", files)
	}
	if items := indexAll(t, cfg); len(items) != 0 {
		t.Errorf("index must stay empty, got %v", items)
	}
}
