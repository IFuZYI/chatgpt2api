package service

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// R2 客户端对真实 S3 兼容服务的往返测试。
//
// 默认跳过：需要一个运行中的 S3 兼容端点（MinIO / LocalStack / 真实 R2）。
// 通过环境变量开启：
//
//	TEST_S3_ENDPOINT=http://127.0.0.1:19000 \
//	TEST_S3_ACCESS_KEY=testkey \
//	TEST_S3_SECRET_KEY=testsecret123 \
//	TEST_S3_BUCKET=chatgpt2api-backups \
//	  go test ./internal/service/ -run TestR2ClientLiveRoundTrip -v
//
// 为什么值得单独写：现有备份测试只覆盖「调度时机」与「快照内容构建」，
// 上传/下载这条真正会丢数据的路径从未被真实执行过 ——
// SigV4 签名、canonical request、分片上传任何一处写错，单测都发现不了。
func TestR2ClientLiveRoundTrip(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("TEST_S3_ENDPOINT"))
	if endpoint == "" {
		t.Skip("未设置 TEST_S3_ENDPOINT，跳过真实 S3 往返测试")
	}
	access := os.Getenv("TEST_S3_ACCESS_KEY")
	secret := os.Getenv("TEST_S3_SECRET_KEY")
	bucket := os.Getenv("TEST_S3_BUCKET")
	if access == "" || secret == "" || bucket == "" {
		t.Skip("缺少 TEST_S3_ACCESS_KEY / TEST_S3_SECRET_KEY / TEST_S3_BUCKET")
	}

	client := NewR2Client(BackupSettings{
		AccountID:   "local",
		AccessKeyID: access,
		SecretKey:   secret,
		Bucket:      bucket,
		Prefix:      "roundtrip-test",
	})
	client.overrideBase = endpoint
	client.HTTPClient = &http.Client{Timeout: 30 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	key := "roundtrip-test/" + RandomHexTag() + ".txt"
	payload := []byte("chatgpt2api backup round-trip " + time.Now().Format(time.RFC3339))

	if _, err := client.UploadBytes(ctx, key, payload, "text/plain", nil); err != nil {
		t.Fatalf("UploadBytes: %v", err)
	}

	got, err := client.DownloadBytes(ctx, key)
	if err != nil {
		t.Fatalf("DownloadBytes: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("内容不一致:\n got=%q\nwant=%q", got, payload)
	}

	// 清理
	if err := client.DeleteObject(ctx, key); err != nil {
		t.Logf("DeleteObject 失败（不影响结论）: %v", err)
	}
}
