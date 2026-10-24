package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// both 模式的远端上传必须是异步的：不能因为 WebDAV 慢就拖慢生图。
//
// 用户诉求：本地远端都存，但生图不等远端；读取时本地有就用本地，
// 本地没有才回远端。
//
// 这条测试用「远端慢 500ms」验证 SaveImageBytes 立刻返回，
// 而远端上传在后台继续。
func TestBothModeUploadsAsynchronously(t *testing.T) {
	recorder := newWebDAVRecorder()
	recorder.putDelay = 500 * time.Millisecond
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, _ := newImageStorageFixture(t, "both", server.URL)

	start := time.Now()
	rel, url, err := svc.SaveImageBytes(testPNG(t, 2, 2), "http://host")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if rel == "" || url == "" {
		t.Fatalf("rel/url empty: %q %q", rel, url)
	}
	// 远端起决定权时这里会 >= 500ms；异步应当远快于它。
	if elapsed > 300*time.Millisecond {
		t.Errorf("SaveImageBytes took %v, want < 300ms (upload must be async)", elapsed)
	}
}

// 异步上传成功后，索引最终要被更新为 storage=both / webdav=true。
func TestBothModeAsyncUploadUpdatesIndex(t *testing.T) {
	recorder := newWebDAVRecorder()
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, cfg := newImageStorageFixture(t, "both", server.URL)

	rel, _, err := svc.SaveImageBytes(testPNG(t, 2, 2), "http://host")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	// 刚保存时是 local（远端还没传完）
	items := indexAll(t, cfg)
	if got := stringValue(items[rel]["storage"]); got != "local" {
		t.Logf("immediately after save: storage=%q (may already be both if upload was fast)", got)
	}

	// 等后台上传完成
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		items = indexAll(t, cfg)
		if stringValue(items[rel]["storage"]) == "both" && truthy(items[rel]["webdav"]) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	items = indexAll(t, cfg)
	t.Fatalf("index never updated to both/webdav=true; got storage=%q webdav=%v",
		stringValue(items[rel]["storage"]), truthy(items[rel]["webdav"]))
}

// 远端上传失败时，索引保持 local（不撒谎），且本地图仍可读。
func TestBothModeAsyncUploadFailureKeepsLocal(t *testing.T) {
	recorder := newWebDAVRecorder()
	recorder.putStatus = http.StatusUnauthorized
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, cfg := newImageStorageFixture(t, "both", server.URL)

	rel, _, err := svc.SaveImageBytes(testPNG(t, 2, 2), "http://host")
	if err != nil {
		t.Fatalf("save must succeed (local copy exists): %v", err)
	}

	// 给后台失败一点时间
	time.Sleep(300 * time.Millisecond)

	items := indexAll(t, cfg)
	if got := stringValue(items[rel]["storage"]); got != "local" {
		t.Errorf("storage = %q, want local (remote failed)", got)
	}
	if truthy(items[rel]["webdav"]) {
		t.Error("webdav = true, want false")
	}

	if payload, _, readErr := svc.ReadImage(rel); readErr != nil || len(payload) == 0 {
		t.Errorf("local image must stay readable: err=%v len=%d", readErr, len(payload))
	}
}

// webdav 模式仍必须同步等待（本地没有副本，必须知道远端成不成）。
func TestWebDAVOnlyModeStillSynchronous(t *testing.T) {
	recorder := newWebDAVRecorder()
	recorder.putDelay = 200 * time.Millisecond
	server := httptest.NewServer(recorder.handler())
	defer server.Close()

	svc, _ := newImageStorageFixture(t, "webdav", server.URL)

	start := time.Now()
	if _, _, err := svc.SaveImageBytes(testPNG(t, 2, 2), "http://host"); err != nil {
		t.Fatalf("save: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < 150*time.Millisecond {
		t.Errorf("webdav mode returned in %v, want >= 200ms (must wait for remote)", elapsed)
	}
	if puts, _ := recorder.counts(); puts == 0 {
		t.Error("no remote PUT made")
	}
}
