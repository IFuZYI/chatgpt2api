package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestSyncToWebDAVKeepsConcurrentIndexWrites 是「同步丢索引」的回归用例。
//
// 回归点：SyncToWebDAV 在网络循环之前取一份索引快照，循环结束后把整份
// 快照写回。期间 SaveImageBytes 新增的条目会被抹掉。webdav 模式没有本地
// 副本，索引丢了图就永久不可达（ReadImage 只在索引标记 webdav 时才回源）。
//
// 这条测试让 WebDAV 的 PUT 阻塞住，在阻塞窗口内并发写一条新索引，
// 然后放行，断言新条目仍在。
func TestSyncToWebDAVKeepsConcurrentIndexWrites(t *testing.T) {
	putStarted := make(chan struct{}, 1)
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		case "PUT":
			select {
			case putStarted <- struct{}{}:
			default:
			}
			<-release // 卡住网络循环
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	svc, _ := newImageStorageFixture(t, "webdav", server.URL+"/dav")

	// 先放一张图，让同步有东西可传。webdav 模式下 SaveImageBytes 自己会
	// 打 PUT，所以这里直接用 local 模式种图、再切 webdav 语义太绕；
	// 改为直接写盘 + 登记索引，模拟「本地已有待同步的图」。
	rel := "2026/10/24/existing.png"
	localPath := filepath.Join(svc.cfg.ImagesDir(), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(localPath, testPNG(t, 2, 2), 0o644); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	svc.indexMu.Lock()
	idx := svc.loadIndexLocked()
	idx[rel] = map[string]any{"rel": rel, "path": rel, "storage": "local"}
	svc.saveIndexLocked(idx)
	svc.indexMu.Unlock()

	settings := map[string]any{
		"enabled":          true,
		"mode":             "webdav",
		"webdav_url":       server.URL + "/dav",
		"webdav_username":  "",
		"webdav_password":  "secret",
		"webdav_root_path": "chatgpt2api/images",
		"public_base_url":  "",
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.SyncToWebDAV(context.Background(), settings)
	}()

	// 等同步进入 PUT（此时它已经取过快照、还没写回）。
	select {
	case <-putStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("同步没有进入 PUT 阶段")
	}

	// 在阻塞窗口内并发写一条新索引 —— 模拟 SaveImageBytes。
	newRel := "2026/10/24/concurrent.png"
	svc.indexMu.Lock()
	fresh := svc.loadIndexLocked()
	fresh[newRel] = map[string]any{"rel": newRel, "path": newRel, "storage": "local"}
	svc.saveIndexLocked(fresh)
	svc.indexMu.Unlock()

	close(release)
	wg.Wait()

	svc.indexMu.Lock()
	final := svc.loadIndexLocked()
	svc.indexMu.Unlock()

	if _, ok := final[newRel]; !ok {
		t.Fatalf("同步把并发写入的索引条目 %q 抹掉了（丢更新）", newRel)
	}
	if _, ok := final[rel]; !ok {
		t.Fatalf("同步后原有条目 %q 不见了", rel)
	}
}
