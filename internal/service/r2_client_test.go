package service

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// SigV4 签名必须稳定：同一输入产出同一签名（不含时间的话）。
// 这里验证结构正确性：Authorization 头的四段齐全、SignedHeaders 排序。
func TestSignV4Structure(t *testing.T) {
	client := &R2Client{
		AccountID: "acct", AccessKey: "AKIA", SecretKey: "secret", Bucket: "bucket", Prefix: "backups",
	}
	_, headers := client.signV4("GET", "/bucket", map[string]string{
		"list-type": "2", "max-keys": "1",
	}, nil, nil)

	auth, ok := headers["authorization"]
	if !ok {
		t.Fatal("authorization header missing")
	}
	for _, part := range []string{"AWS4-HMAC-SHA256", "Credential=AKIA/", "SignedHeaders=", "Signature="} {
		if !strings.Contains(auth, part) {
			t.Errorf("authorization missing %q: %s", part, auth)
		}
	}
	// region 必须是 auto（R2 特性）
	if !strings.Contains(auth, "/auto/s3/aws4_request") {
		t.Errorf("credential scope should use region auto: %s", auth)
	}
	// SignedHeaders 必须按字母序
	signed := auth[strings.Index(auth, "SignedHeaders=")+len("SignedHeaders="):]
	signed = signed[:strings.Index(signed, ",")]
	parts := strings.Split(signed, ";")
	for i := 1; i < len(parts); i++ {
		if parts[i-1] > parts[i] {
			t.Errorf("SignedHeaders not sorted: %v", parts)
		}
	}
	if headers["host"] != "acct.r2.cloudflarestorage.com" {
		t.Errorf("host = %q", headers["host"])
	}
	if headers["x-amz-content-sha256"] == "" || headers["x-amz-date"] == "" {
		t.Error("required amz headers missing")
	}
}

func TestEncodeQuerySorted(t *testing.T) {
	got := encodeQuerySorted(map[string]string{"b": "2", "a": "1"})
	if got != "a=1&b=2" {
		t.Errorf("query = %q, want sorted", got)
	}
	if encodeQuerySorted(nil) != "" {
		t.Error("empty query should be empty string")
	}
}

func TestCollapseSpaces(t *testing.T) {
	if got := collapseSpaces("a   b\t c"); got != "a b c" {
		t.Errorf("collapseSpaces = %q", got)
	}
}

func TestR2Validate(t *testing.T) {
	client := NewR2Client(BackupSettings{})
	err := client.Validate()
	if err == nil {
		t.Fatal("empty settings should fail validation")
	}
	// 文案要列出全部缺失项
	for _, field := range []string{"Account ID", "Access Key ID", "Secret Access Key", "Bucket"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error should mention %q: %v", field, err)
		}
	}

	complete := NewR2Client(BackupSettings{
		AccountID: "a", AccessKeyID: "b", SecretKey: "c", Bucket: "d",
	})
	if err := complete.Validate(); err != nil {
		t.Errorf("complete settings should validate: %v", err)
	}
}

func TestIsBackupObject(t *testing.T) {
	positives := []string{
		"backups/backup-20261023T120000Z-ab12.tar.gz",
		"backups/backup-20261023T120000Z-ab12.tar.gz.enc",
	}
	for _, key := range positives {
		if !IsBackupObject(key) {
			t.Errorf("%q should be a backup object", key)
		}
	}
	negatives := []string{
		"backups/other.tar.gz",
		"backups/backup-x.zip",
		"backups/readme.txt",
	}
	for _, key := range negatives {
		if IsBackupObject(key) {
			t.Errorf("%q should not be a backup object", key)
		}
	}
}

func TestBackupSummary(t *testing.T) {
	summary := BackupSummary(map[string]any{
		"key":        "backups/backup-20261023T120000Z-ab12.tar.gz.enc",
		"size":       1024,
		"updated_at": "2026-10-23T12:00:00Z",
	})
	if summary["name"] != "backup-20261023T120000Z-ab12.tar.gz.enc" {
		t.Errorf("name = %v", summary["name"])
	}
	if summary["encrypted"] != true {
		t.Errorf("encrypted flag wrong: %v", summary["encrypted"])
	}
	if summary["size"] != 1024 {
		t.Errorf("size = %v", summary["size"])
	}
}

// ListObjects 要能解析 S3 的 XML 响应并处理分页。
func TestR2ListObjectsParsesXML(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0"?>
<ListBucketResult>
  <Contents><Key>backups/backup-1.tar.gz</Key><Size>100</Size><LastModified>2026-10-23T10:00:00Z</LastModified></Contents>
  <IsTruncated>true</IsTruncated>
  <NextContinuationToken>tok</NextContinuationToken>
</ListBucketResult>`))
			return
		}
		w.Write([]byte(`<?xml version="1.0"?>
<ListBucketResult>
  <Contents><Key>backups/backup-2.tar.gz</Key><Size>200</Size><LastModified>2026-10-23T11:00:00Z</LastModified></Contents>
  <IsTruncated>false</IsTruncated>
</ListBucketResult>`))
	}))
	defer server.Close()

	client := NewR2Client(BackupSettings{
		AccountID: "acct", AccessKeyID: "a", SecretKey: "b", Bucket: "bucket", Prefix: "backups",
	})
	// 把 endpoint 指到测试服务器
	client.AccountID = "acct"
	overrideEndpoint(client, server.URL)

	items, err := client.ListObjects(context.Background())
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	// 按 updated_at 倒序：backup-2 更新，应排第一
	if !strings.Contains(stringValue(items[0]["key"]), "backup-2") {
		t.Errorf("items not sorted desc: %v", items[0]["key"])
	}
	if calls != 2 {
		t.Errorf("pagination calls = %d, want 2", calls)
	}
}

// XML 解析器本身要能处理 S3 的命名空间前缀。
func TestListBucketResultXML(t *testing.T) {
	payload := `<?xml version="1.0"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Contents><Key>a/b.tar.gz</Key><Size>42</Size><LastModified>2026-01-01T00:00:00Z</LastModified></Contents>
</ListBucketResult>`
	var result listBucketResult
	if err := xml.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Contents) != 1 || result.Contents[0].Key != "a/b.tar.gz" {
		t.Fatalf("parsed wrong: %+v", result)
	}
}
