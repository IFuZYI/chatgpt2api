package service

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// WebDAV 客户端对真实 WebDAV 服务的往返测试。
//
// 默认跳过，用环境变量开启：
//
//	TEST_WEBDAV_URL=http://127.0.0.1:18080 \
//	TEST_WEBDAV_USER=davuser TEST_WEBDAV_PASS=davpass123 \
//	  go test ./internal/service/ -run TestWebDAVLiveRoundTrip -v
//
// 为什么需要：现有 WebDAV 测试全部基于自写的 webdavFake。fake 与真实
// WebDAV 服务在以下语义上可能不一致，而它们恰恰是生产会踩的：
//   - MKCOL 已存在时返回 405（fake 可能返回 201）
//   - PUT 需要逐级目录先存在
//   - PROPFIND 的 Depth 语义
//   - URL 段转义（文件名含空格/中文/括号）
//
// 备份链路就是靠这类真实往返测试抓到了 SigV4 的 403。
func TestWebDAVLiveRoundTrip(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("TEST_WEBDAV_URL"))
	if base == "" {
		t.Skip("未设置 TEST_WEBDAV_URL，跳过真实 WebDAV 往返测试")
	}
	client := NewWebDAVClient(map[string]any{
		"webdav_url":       base,
		"webdav_username":  os.Getenv("TEST_WEBDAV_USER"),
		"webdav_password":  os.Getenv("TEST_WEBDAV_PASS"),
		"webdav_root_path": "chatgpt2api-test",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 用带空格与中文的文件名，顺带验证逐段转义。
	rel := "2026/10/27/测试 image (1).png"
	payload := []byte("fake-png-bytes-" + time.Now().Format(time.RFC3339))

	publicURL, err := client.Put(ctx, rel, payload, "")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if publicURL == "" {
		t.Log("Put 未返回 public URL（未配置 public_base_url 时正常）")
	}

	got, err := client.Get(ctx, rel)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("内容不一致:\n got=%q\nwant=%q", got, payload)
	}

	deleted, err := client.Delete(ctx, rel)
	if err != nil {
		t.Logf("Delete 失败（不影响结论）: %v", err)
	} else if !deleted {
		t.Log("Delete 返回 false（对象可能已不存在）")
	}
}

// 重复 Put 同一路径必须幂等覆盖（真实 WebDAV 对已存在资源返回 204/200，
// 而不是 fake 里常见的 201）。
func TestWebDAVLiveOverwriteIsIdempotent(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("TEST_WEBDAV_URL"))
	if base == "" {
		t.Skip("未设置 TEST_WEBDAV_URL")
	}
	client := NewWebDAVClient(map[string]any{
		"webdav_url":       base,
		"webdav_username":  os.Getenv("TEST_WEBDAV_USER"),
		"webdav_password":  os.Getenv("TEST_WEBDAV_PASS"),
		"webdav_root_path": "chatgpt2api-test",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rel := "overwrite-test.txt"
	if _, err := client.Put(ctx, rel, []byte("first"), ""); err != nil {
		t.Fatalf("首次 Put: %v", err)
	}
	if _, err := client.Put(ctx, rel, []byte("second"), ""); err != nil {
		t.Fatalf("覆盖 Put: %v", err)
	}
	got, err := client.Get(ctx, rel)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("覆盖后内容 = %q, want %q", got, "second")
	}
	_, _ = client.Delete(ctx, rel)
}
