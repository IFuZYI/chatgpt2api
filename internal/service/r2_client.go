package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// R2Client 是 Cloudflare R2（S3 兼容）客户端，复刻 services/backup_service.py 的
// CloudflareR2Client。
//
// 这里刻意手写 AWS SigV4 而不是引入 SDK：
//   - 只用到 4 个操作（PutObject / GetObject / DeleteObject / ListObjectsV2）
//   - 引入 SDK 会带来几十 MB 依赖，而签名逻辑只有 ~60 行
//   - Python 版也是手写的，行为已对齐
type R2Client struct {
	AccountID  string
	AccessKey  string
	SecretKey  string
	Bucket     string
	Prefix     string
	HTTPClient *http.Client

	// overrideBase 仅测试用：把 endpoint 指到本地服务器。
	overrideBase string
}

// NewR2Client 从备份配置构造客户端。
func NewR2Client(settings BackupSettings) *R2Client {
	prefix := strings.TrimSpace(settings.Prefix)
	if prefix == "" {
		prefix = "backups"
	}
	return &R2Client{
		AccountID:  strings.TrimSpace(settings.AccountID),
		AccessKey:  strings.TrimSpace(settings.AccessKeyID),
		SecretKey:  strings.TrimSpace(settings.SecretKey),
		Bucket:     strings.TrimSpace(settings.Bucket),
		Prefix:     prefix,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// Validate 检查必填项，缺什么报什么（文案与 Python 一致）。
func (c *R2Client) Validate() error {
	missing := make([]string, 0, 4)
	if c.AccountID == "" {
		missing = append(missing, "Account ID")
	}
	if c.AccessKey == "" {
		missing = append(missing, "Access Key ID")
	}
	if c.SecretKey == "" {
		missing = append(missing, "Secret Access Key")
	}
	if c.Bucket == "" {
		missing = append(missing, "Bucket")
	}
	if len(missing) > 0 {
		return newBackupError("R2 配置不完整：缺少 %s", strings.Join(missing, "、"))
	}
	return nil
}

func (c *R2Client) endpoint() string {
	if c.overrideBase != "" {
		return c.overrideBase
	}
	return "https://" + c.AccountID + ".r2.cloudflarestorage.com"
}

func (c *R2Client) host() string {
	return c.AccountID + ".r2.cloudflarestorage.com"
}

// signV4 生成 AWS SigV4 签名头。
//
// 关键点（照抄 Python 的 _aws_v4_headers）：
//   - canonical query 必须按 key 排序并 URL 编码
//   - canonical headers 按 key 排序，值要压掉多余空白
//   - region 固定 "auto"（R2 的特性，不是标准 region）
func (c *R2Client) signV4(
	method, objectPath string,
	query map[string]string,
	body []byte,
	extraHeaders map[string]string,
) (string, map[string]string) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	encodedQuery := encodeQuerySorted(query)
	payloadHash := sha256Hex(body)

	headers := map[string]string{
		"host":                 c.host(),
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	for key, value := range extraHeaders {
		headers[strings.ToLower(key)] = strings.TrimSpace(value)
	}

	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var canonicalHeaders strings.Builder
	for _, key := range keys {
		canonicalHeaders.WriteString(key + ":" + collapseSpaces(headers[key]) + "\n")
	}
	signedHeaders := strings.Join(keys, ";")

	canonicalRequest := strings.Join([]string{
		strings.ToUpper(method),
		objectPath,
		encodedQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := dateStamp + "/auto/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+c.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, "auto")
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	authorization := "AWS4-HMAC-SHA256 " +
		"Credential=" + c.AccessKey + "/" + credentialScope + ", " +
		"SignedHeaders=" + signedHeaders + ", " +
		"Signature=" + signature

	out := make(map[string]string, len(headers)+1)
	for key, value := range headers {
		out[key] = value
	}
	out["authorization"] = authorization
	return encodedQuery, out
}

func encodeQuerySorted(query map[string]string) string {
	if len(query) == 0 {
		return ""
	}
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(query[key]))
	}
	return strings.Join(parts, "&")
}

// collapseSpaces 把连续空白压成单个空格（SigV4 要求）。
func collapseSpaces(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func sha256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

// request 发一次签名请求。
func (c *R2Client) request(
	ctx context.Context,
	method, key string,
	query map[string]string,
	body []byte,
	extraHeaders map[string]string,
) (*http.Response, error) {
	objectPath := "/" + c.Bucket
	if key != "" {
		// key 里的 / 要保留，其余转义。
		escaped := strings.ReplaceAll(url.PathEscape(strings.TrimLeft(key, "/")), "%2F", "/")
		objectPath += "/" + escaped
	}

	encodedQuery, headers := c.signV4(method, objectPath, query, body, extraHeaders)
	target := c.endpoint() + objectPath
	if encodedQuery != "" {
		target += "?" + encodedQuery
	}

	var reader io.Reader
	if len(body) > 0 {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), target, reader)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return c.HTTPClient.Do(req)
}

// TestConnection 发一次 ListObjectsV2（max-keys=1）验证配置。
func (c *R2Client) TestConnection(ctx context.Context) (map[string]any, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	resp, err := c.request(ctx, http.MethodGet, "", map[string]string{
		"list-type": "2",
		"max-keys":  "1",
	}, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return nil, newBackupError("连接 R2 失败：HTTP %d", resp.StatusCode)
	}
	return map[string]any{"ok": true, "status": resp.StatusCode}, nil
}

// UploadBytes 上传对象。
func (c *R2Client) UploadBytes(ctx context.Context, key string, payload []byte, contentType string, metadata map[string]string) (map[string]any, error) {
	headers := map[string]string{"content-type": contentType}
	for name, value := range metadata {
		headers["x-amz-meta-"+name] = value
	}
	resp, err := c.request(ctx, http.MethodPut, key, nil, payload, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return nil, newBackupError("上传备份失败：HTTP %d", resp.StatusCode)
	}
	return map[string]any{
		"key":  key,
		"etag": strings.Trim(resp.Header.Get("etag"), `"`),
	}, nil
}

// DeleteObject 删除对象（404 视为成功，与 Python 一致）。
func (c *R2Client) DeleteObject(ctx context.Context, key string) error {
	resp, err := c.request(ctx, http.MethodDelete, key, nil, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return newBackupError("删除备份失败：HTTP %d", resp.StatusCode)
	}
	return nil
}

// DownloadBytes 下载对象。
func (c *R2Client) DownloadBytes(ctx context.Context, key string) ([]byte, error) {
	resp, err := c.request(ctx, http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, newBackupError("读取备份失败：HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 512*1024*1024))
}

// listBucketResult 是 ListObjectsV2 的响应结构。
type listBucketResult struct {
	Contents []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// ListObjects 分页列出 prefix 下的全部对象，按 updated_at 倒序。
//
// Python 用字符串切分解析 XML；这里用 encoding/xml，语义相同但更稳。
func (c *R2Client) ListObjects(ctx context.Context) ([]map[string]any, error) {
	items := make([]map[string]any, 0, 32)
	continuation := ""

	for {
		query := map[string]string{
			"list-type": "2",
			"prefix":    strings.TrimRight(c.Prefix, "/") + "/",
			"max-keys":  "1000",
		}
		if continuation != "" {
			query["continuation-token"] = continuation
		}

		resp, err := c.request(ctx, http.MethodGet, "", query, nil, nil)
		if err != nil {
			return nil, err
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode >= 400 {
			return nil, newBackupError("获取备份列表失败：HTTP %d", resp.StatusCode)
		}

		var result listBucketResult
		if err := xml.Unmarshal(payload, &result); err != nil {
			return nil, newBackupError("解析备份列表失败：%v", err)
		}
		for _, entry := range result.Contents {
			key := strings.TrimSpace(entry.Key)
			if key == "" {
				continue
			}
			items = append(items, map[string]any{
				"key":        key,
				"size":       entry.Size,
				"updated_at": strings.TrimSpace(entry.LastModified),
			})
		}
		if !result.IsTruncated || result.NextContinuationToken == "" {
			break
		}
		continuation = result.NextContinuationToken
	}

	sort.SliceStable(items, func(i, j int) bool {
		return stringValue(items[i]["updated_at"]) > stringValue(items[j]["updated_at"])
	})
	return items, nil
}

// ParseSize 容错解析尺寸字符串。
func ParseSize(value string) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// IsBackupObject 复刻 _is_backup_object：只认 backup-*.tar.gz[.enc]。
func IsBackupObject(key string) bool {
	name := key
	if index := strings.LastIndexByte(key, '/'); index >= 0 {
		name = key[index+1:]
	}
	if !strings.HasPrefix(name, "backup-") {
		return false
	}
	return strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tar.gz.enc")
}

// BackupSummary 复刻 list_backups 的条目形状。
func BackupSummary(item map[string]any) map[string]any {
	key := stringValue(item["key"])
	name := key
	if index := strings.LastIndexByte(key, '/'); index >= 0 {
		name = key[index+1:]
	}
	return map[string]any{
		"key":        key,
		"name":       name,
		"size":       intValue(item["size"]),
		"updated_at": item["updated_at"],
		"encrypted":  strings.HasSuffix(name, ".enc"),
	}
}

var _ = fmt.Sprintf
