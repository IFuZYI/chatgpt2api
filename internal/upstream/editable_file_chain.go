package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"

	"github.com/IFuZYI/chatgpt2api/internal/protocol"
)

// 可编辑文件（PPT/PSD）链路：复刻 services/openai_backend_api.py:1163-1780。
//
// 与图片链路同源（prepare 换 conduit_token → /f/conversation 建会话），差别：
//   - 模型固定 gpt-5-5-thinking，思考强度 extended
//   - 参考图必须先上传（POST /backend-api/files → PUT upload_url → .../uploaded）
//   - 产物不是 SSE 里的图片引用，而是轮询会话文档拿到的文件（pptx/psd + zip）
//
// 因此这里复用 imageHeaders / readSSEPayloads / chatRequirements，
// 只新增「上传」「文件产物抽取」「文件下载」三段。

const (
	// editableThinkingEffort 复刻 EDITABLE_FILE_THINKING_EFFORT。
	editableThinkingEffort = "extended"
	// editableTimeout 复刻 EDITABLE_FILE_TIMEOUT_SECS（1200s）。
	editableTimeout = 1200 * time.Second
	// editablePollInterval 复刻 EDITABLE_FILE_POLL_INTERVAL_SECS（5s）。
	editablePollInterval = 5 * time.Second

	editablePSDDefaultExtension = ".psd"
	editablePPTDefaultExtension = ".pptx"
)

// 两个固定 prompt 模板，逐字取自 services/openai_backend_api.py:87-92。
//
// 不要改一个字：上游模型是按这段指令决定「拆素材 → 合成可编辑文件」流程的，
// 改写会让它退化成普通对话。测试用 sha256 断言长度与内容。
const editablePPTDefaultPrompt = `我需要你根据用户的需求，来制作一个可以编辑的PPT，你可以使用Agent来做，你不要再继续询问用户问题，内容风格、版式、配色、内容结构和页面信息你可以自行补充并直接执行。整体的流程如下：
1. 用生图的方式，帮我生成一个精美的产品介绍ppt，5-6个页面
2. 帮我把以上涉及到的所有图像和形状素材拆分成单独png，每个素材单独一张图片，不要有遗漏，让我可以直接在ppt里拼接素材还原，不要文字
3. 利用以上所有图片和形状素材，帮我还原你第一次生成的展示ppt，我需要是可编辑的ppt格式，主要部分需要你单独还原插入，文字需要可以编辑
最后只需要给我生成一个PPT文件，以及生成中遇到的各种素材压缩包zip文件就行。`

const editablePSDDefaultPrompt = `帮我生成这个图像，把这张海报分成若干图像，包括背景图，每个元素不要改位置，这样子我可以直接在 平时里无需拖动，底色为白色，不要伪透明底。再帮我将以上拆分的图像拼合成一个psd文件，去除白色底，不要改变每个图层的相应位置，保留每个元素所在图层的相应位置，保留每个元素的图层，最后只需要给我输出psd文件，以及每个图层的zip文件`

var (
	// editableAssetPointerRe 复刻 EDITABLE_ASSET_POINTER_RE。
	editableAssetPointerRe = regexp.MustCompile(`(?:file-service|sediment)://([A-Za-z0-9_-]+)`)
	// editableFileIDRe 复刻 FILE_ID_RE。
	//
	// Python 的原文是 \b(file[-_](?!service\b)[A-Za-z0-9_-]+)\b —— Go 的 RE2
	// 不支持负向前瞻，所以这里先匹配，再把恰好是 file-service / file_service
	// 的命中过滤掉（editableMatchFileID / editableExtractFileIDs 里做）。
	editableFileIDRe = regexp.MustCompile(`\b(file[-_][A-Za-z0-9_-]+)\b`)
	// editableDataURIRe 复刻 _decode_editable_base64_image 里的 data: 前缀解析。
	editableDataURIRe = regexp.MustCompile(`(?is)^data:([^;]+);base64,(.*)$`)
	// editablePSDExportFileRe / editablePPTExportFileRe 复刻两条导出路径正则。
	//
	// 差异：Python 的 \s 在 str 模式含 Unicode 空白，Go 的 \s 只有 ASCII。
	// 产物路径是 ASCII 沙箱路径，这个差异不会影响匹配。
	editablePSDExportFileRe = regexp.MustCompile(`(?i)(?:sandbox:)?(/mnt/data/[^\s"'\)\]]+\.(?:psd|zip))`)
	editablePPTExportFileRe = regexp.MustCompile(`(?i)(?:sandbox:)?(/mnt/data/[^\s"'\)\]]+\.(?:pptx?|zip))`)
)

var (
	editableZipMimeTypes = map[string]bool{
		"application/zip":              true,
		"application/x-zip-compressed": true,
	}
	editablePSDMimeTypes = map[string]bool{
		"image/vnd.adobe.photoshop":       true,
		"application/vnd.adobe.photoshop": true,
	}
	editablePPTMimeTypes = map[string]bool{
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
		"application/vnd.ms-powerpoint":                                             true,
	}
)

// editableKindSpec 是「主产物长什么样」的描述，对应 Python 传给
// _export_editable_file_zip 的那组关键字参数。
type editableKindSpec struct {
	label            string
	suffixes         []string
	mimeTypes        map[string]bool
	mimeKeywords     []string
	defaultExtension string
	exportFileRe     *regexp.Regexp
	fixedPrompt      string
}

// editableSpecFor 按 kind 取产物规格；未知 kind 返回零值（label 为空）。
func editableSpecFor(kind string) editableKindSpec {
	switch kind {
	case protocol.EditableKindPSD:
		return editableKindSpec{
			label:            "psd",
			suffixes:         []string{".psd"},
			mimeTypes:        editablePSDMimeTypes,
			mimeKeywords:     []string{"photoshop"},
			defaultExtension: editablePSDDefaultExtension,
			exportFileRe:     editablePSDExportFileRe,
			fixedPrompt:      editablePSDDefaultPrompt,
		}
	case protocol.EditableKindPPT:
		return editableKindSpec{
			label:            "ppt",
			suffixes:         []string{".ppt", ".pptx"},
			mimeTypes:        editablePPTMimeTypes,
			mimeKeywords:     []string{"presentationml.presentation", "ms-powerpoint"},
			defaultExtension: editablePPTDefaultExtension,
			exportFileRe:     editablePPTExportFileRe,
			fixedPrompt:      editablePPTDefaultPrompt,
		}
	default:
		return editableKindSpec{}
	}
}

// editableOptions 让测试压缩超时与轮询间隔；生产值见上面的常量。
type editableOptions struct {
	timeout      time.Duration
	pollInterval time.Duration
}

// editableArtifact 复刻 EditableFileArtifact（:135-144）。
type editableArtifact struct {
	attachmentID string
	fileID       string
	name         string
	mimeType     string
	createTime   float64
	authorRole   string
	sandboxPath  string
	messageID    string
}

// editableUploadedImage 是上传成功后回传给 conversation 的元数据。
type editableUploadedImage struct {
	fileID        string
	libraryFileID string
	fileName      string
	fileSize      int
	mimeType      string
	width         int
	height        int
}

// ExportEditableFile 实现 protocol.EditableFileUpstream：
// 上传参考图 → prepare → conversation → 轮询产物 → 下载。
func (c *Client) ExportEditableFile(
	ctx context.Context,
	request protocol.EditableFileRequest,
) (protocol.EditableFileExportResult, error) {
	return c.exportEditableFile(ctx, request, editableOptions{
		timeout:      editableTimeout,
		pollInterval: editablePollInterval,
	})
}

// exportEditableFile 复刻 _export_editable_file_zip（:1213-1261）。
func (c *Client) exportEditableFile(
	ctx context.Context,
	request protocol.EditableFileRequest,
	options editableOptions,
) (protocol.EditableFileExportResult, error) {
	spec := editableSpecFor(request.Kind)
	if spec.label == "" {
		return protocol.EditableFileExportResult{}, fmt.Errorf("unknown editable file kind: %q", request.Kind)
	}
	if strings.TrimSpace(c.accessToken) == "" {
		return protocol.EditableFileExportResult{}, errors.New("access_token is required for editable file export")
	}
	// PSD 链路必须有参考图（Python 在 export_psd_zip 里就拦住）。
	if request.Kind == protocol.EditableKindPSD && len(request.Base64Images) == 0 {
		return protocol.EditableFileExportResult{}, errors.New("base64_images is empty")
	}

	// 这条链路用专门的 client version/build（Python 直接改 session.headers）。
	// 不改的话上游会按当前 web 版本处理，产物链接与 interpreter 端点行为都可能不同。
	c.clientVersion = EditableClientVersion
	c.clientBuild = EditableClientBuild

	if err := os.MkdirAll(request.OutputDir, 0o755); err != nil {
		return protocol.EditableFileExportResult{}, err
	}

	uploaded := make([]editableUploadedImage, 0, len(request.Base64Images))
	for index, imagePayload := range request.Base64Images {
		item, err := c.uploadEditableBase64Image(ctx, imagePayload, index+1)
		if err != nil {
			return protocol.EditableFileExportResult{}, err
		}
		uploaded = append(uploaded, item)
	}

	prompt := editablePrompt(spec.fixedPrompt, request.Prompt)

	mimeTypes := make([]string, 0, len(uploaded))
	for _, item := range uploaded {
		mimeTypes = append(mimeTypes, item.mimeType)
	}
	conduitToken, err := c.prepareEditableConversation(ctx, prompt, mimeTypes)
	if err != nil {
		return protocol.EditableFileExportResult{}, err
	}

	conversationID, err := c.runEditableConversation(ctx, prompt, uploaded, conduitToken)
	if err != nil {
		return protocol.EditableFileExportResult{}, err
	}

	artifacts, err := c.waitEditableOutputArtifacts(ctx, conversationID, spec, options)
	if err != nil {
		return protocol.EditableFileExportResult{}, err
	}

	downloaded := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		savedPath, downloadErr := c.downloadEditableArtifact(ctx, conversationID, artifact, request.OutputDir, spec)
		if downloadErr != nil {
			return protocol.EditableFileExportResult{}, downloadErr
		}
		downloaded = append(downloaded, savedPath)
	}

	primaryPath := ""
	zipPath := ""
	for _, savedPath := range downloaded {
		lower := strings.ToLower(filepath.Ext(savedPath))
		if primaryPath == "" && editableHasSuffix(lower, spec.suffixes) {
			primaryPath = savedPath
		}
		if zipPath == "" && lower == ".zip" {
			zipPath = savedPath
		}
	}
	if primaryPath == "" || zipPath == "" {
		return protocol.EditableFileExportResult{}, fmt.Errorf(
			"download finished but did not get both %s and zip files: %v", spec.label, downloaded)
	}
	return protocol.EditableFileExportResult{
		ConversationID: conversationID,
		PrimaryPath:    primaryPath,
		ZipPath:        zipPath,
	}, nil
}

// editablePrompt 复刻 _editable_prompt（:1163-1166）：
// 用户补充需求为空时只用固定模板，否则追加一段引导语。
func editablePrompt(fixedPrompt, userPrompt string) string {
	extra := strings.TrimSpace(userPrompt)
	if extra == "" {
		return fixedPrompt
	}
	return fixedPrompt + "\n\n以下是用户补充需求，请直接结合执行：\n" + extra
}

// uploadEditableBase64Image 复刻 _upload_editable_base64_image（:1263-1318）。
func (c *Client) uploadEditableBase64Image(ctx context.Context, base64Image string, index int) (editableUploadedImage, error) {
	data, fileName, mimeType, width, height, err := decodeEditableBase64Image(base64Image, index)
	if err != nil {
		return editableUploadedImage{}, err
	}

	createBody, err := json.Marshal(map[string]any{
		"file_name":                fileName,
		"file_size":                len(data),
		"use_case":                 "multimodal",
		"timezone_offset_min":      -480,
		"reset_rate_limits":        false,
		"store_in_library":         true,
		"library_persistence_mode": "opportunistic",
	})
	if err != nil {
		return editableUploadedImage{}, err
	}

	path := "/backend-api/files"
	responseBody, status, err := c.doWithHeaders(ctx, http.MethodPost, BaseURL+path, path, map[string]string{
		"Accept":       "*/*",
		"Content-Type": "application/json",
	}, createBody, 60*time.Second)
	if err != nil {
		return editableUploadedImage{}, err
	}
	if status >= 400 {
		return editableUploadedImage{}, NewStatusError(status, nil,
			fmt.Sprintf("%s failed: status=%d body=%s", path, status, truncate(string(responseBody), 300)))
	}

	var created map[string]any
	if err := json.Unmarshal(responseBody, &created); err != nil {
		return editableUploadedImage{}, fmt.Errorf("%s: invalid json: %w", path, err)
	}
	uploadURL := stringOfValue(created["upload_url"])
	fileID := stringOfValue(created["file_id"])
	if uploadURL == "" || fileID == "" {
		return editableUploadedImage{}, fmt.Errorf("invalid upload response: %s", truncate(string(responseBody), 300))
	}

	// PUT 到 Azure blob 预签名地址：这里刻意用裸头（不是指纹头），
	// 与 Python 一致 —— blob 存储会拒绝带 Authorization 的跨域请求。
	putHeader := http.Header{}
	putHeader.Set("Content-Type", mimeType)
	putHeader.Set("x-ms-blob-type", "BlockBlob")
	putHeader.Set("x-ms-version", "2020-04-08")
	putHeader.Set("Origin", BaseURL)
	putHeader.Set("Referer", BaseURL+"/")
	putHeader.Set("User-Agent", c.userAgent)
	putHeader.Set("Accept", "application/json, text/plain, */*")
	putHeader.Set("Accept-Language", "en-US,en;q=0.8")

	putBody, putStatus, err := c.doWithRawHeaders(ctx, http.MethodPut, uploadURL, putHeader, data, 120*time.Second)
	if err != nil {
		return editableUploadedImage{}, err
	}
	if putStatus >= 400 {
		return editableUploadedImage{}, NewStatusError(putStatus, nil,
			fmt.Sprintf("image_upload failed: status=%d body=%s", putStatus, truncate(string(putBody), 300)))
	}

	finishPath := "/backend-api/files/" + fileID + "/uploaded"
	finishBody, finishStatus, err := c.doWithHeaders(ctx, http.MethodPost, BaseURL+finishPath, finishPath, map[string]string{
		"Accept":       "*/*",
		"Content-Type": "application/json",
	}, []byte("{}"), 60*time.Second)
	if err != nil {
		return editableUploadedImage{}, err
	}
	if finishStatus >= 400 {
		return editableUploadedImage{}, NewStatusError(finishStatus, nil,
			fmt.Sprintf("%s failed: status=%d body=%s", finishPath, finishStatus, truncate(string(finishBody), 300)))
	}

	return editableUploadedImage{
		fileID:        fileID,
		libraryFileID: stringOfValue(created["library_file_id"]),
		fileName:      fileName,
		fileSize:      len(data),
		mimeType:      mimeType,
		width:         width,
		height:        height,
	}, nil
}

// editableImageExtensions 是 mimetypes.guess_extension 在图片 MIME 上的结果。
var editableImageExtensions = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
	"image/bmp":  ".bmp",
	"image/tiff": ".tiff",
}

// editableImageMimeTypes 是 PIL Image.MIME 在已注册格式上的映射。
var editableImageMimeTypes = map[string]string{
	"png":  "image/png",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
	"bmp":  "image/bmp",
	"tiff": "image/tiff",
}

// decodeEditableBase64Image 复刻 _decode_editable_base64_image（:1320-1336）：
// 去掉 data: 前缀、解码、读宽高、按真实格式定 MIME 与扩展名。
func decodeEditableBase64Image(base64Image string, index int) ([]byte, string, string, int, int, error) {
	raw := strings.TrimSpace(base64Image)
	if raw == "" {
		return nil, "", "", 0, 0, errors.New("base64 image is empty")
	}
	mimeType := ""
	payload := raw
	if match := editableDataURIRe.FindStringSubmatch(raw); match != nil {
		mimeType = strings.ToLower(strings.TrimSpace(match[1]))
		payload = strings.TrimSpace(match[2])
	}

	data, err := decodeBase64Image(payload)
	if err != nil {
		return nil, "", "", 0, 0, err
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		// Python 的 Image.open(...).load() 对坏数据同样抛异常。
		return nil, "", "", 0, 0, fmt.Errorf("invalid image data: %w", err)
	}
	if resolved := editableImageMimeTypes[strings.ToLower(format)]; resolved != "" {
		mimeType = resolved
	} else if mimeType == "" {
		mimeType = "image/png"
	}
	extension := editableImageExtensions[mimeType]
	if extension == "" {
		extension = ".png"
	}
	return data, fmt.Sprintf("image_%d%s", index, extension), mimeType, config.Width, config.Height, nil
}

// prepareEditableConversation 复刻 _prepare_editable_conversation（:1338-1368）。
//
// 与图片链路的 prepare 不同：system_hints 是空数组（没有 picture_v2），
// 多了 thinking_effort 与可选的 attachment_mime_types。
func (c *Client) prepareEditableConversation(ctx context.Context, prompt string, attachmentMimeTypes []string) (string, error) {
	payload := map[string]any{
		"action":                "next",
		"fork_from_shared_post": false,
		"parent_message_id":     "client-created-root",
		"model":                 protocol.EditableFileModel,
		"client_prepare_state":  "success",
		"timezone_offset_min":   -480,
		"timezone":              "Asia/Shanghai",
		"conversation_mode":     map[string]any{"kind": "primary_assistant"},
		"system_hints":          []any{},
		"partial_query": map[string]any{
			"id":      newUUID(),
			"author":  map[string]any{"role": "user"},
			"content": map[string]any{"content_type": "text", "parts": []any{prompt}},
		},
		"supports_buffering":     true,
		"supported_encodings":    []any{"v1"},
		"client_contextual_info": map[string]any{"app_name": "chatgpt.com"},
		"thinking_effort":        editableThinkingEffort,
	}
	if len(attachmentMimeTypes) > 0 {
		payload["attachment_mime_types"] = attachmentMimeTypes
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	path := "/backend-api/f/conversation/prepare"
	responseBody, status, err := c.doWithHeaders(ctx, http.MethodPost, BaseURL+path, path, map[string]string{
		"Accept":          "*/*",
		"Content-Type":    "application/json",
		"X-Conduit-Token": "no-token",
	}, body, 60*time.Second)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", NewStatusError(status, retryAfterFromBody(responseBody),
			fmt.Sprintf("%s failed: status=%d body=%s", path, status, truncate(string(responseBody), 300)))
	}

	var decoded map[string]any
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return "", fmt.Errorf("%s: invalid json: %w", path, err)
	}
	conduitToken := stringOfValue(decoded["conduit_token"])
	if conduitToken == "" {
		return "", fmt.Errorf("missing conduit_token: %s", truncate(string(responseBody), 300))
	}
	return conduitToken, nil
}

// runEditableConversation 复刻 _run_editable_conversation（:1370-1449）：
// bootstrap → sentinel → 带 conduit_token 发 SSE，从事件流里找 conversation_id。
func (c *Client) runEditableConversation(
	ctx context.Context,
	prompt string,
	uploaded []editableUploadedImage,
	conduitToken string,
) (string, error) {
	if err := c.Bootstrap(ctx); err != nil {
		return "", err
	}
	rawReq, err := c.chatRequirements(ctx)
	if err != nil {
		return "", err
	}
	requirements := protocol.Requirements{
		Token:          rawReq.Token,
		ProofToken:     rawReq.ProofToken,
		TurnstileToken: rawReq.TurnstileToken,
		SOToken:        rawReq.SOToken,
		Raw:            rawReq.Raw,
	}

	message := map[string]any{
		"id":          newUUID(),
		"author":      map[string]any{"role": "user"},
		"create_time": float64(time.Now().UnixNano()) / 1e9,
	}
	if len(uploaded) > 0 {
		parts := make([]any, 0, len(uploaded)+1)
		attachments := make([]any, 0, len(uploaded))
		for _, item := range uploaded {
			// 注意 asset_pointer 前缀是 sediment://（图片链路用的是 file-service://）。
			parts = append(parts, map[string]any{
				"content_type":  "image_asset_pointer",
				"asset_pointer": "sediment://" + item.fileID,
				"size_bytes":    item.fileSize,
				"width":         item.width,
				"height":        item.height,
			})
			attachments = append(attachments, map[string]any{
				"id":              item.fileID,
				"size":            item.fileSize,
				"name":            item.fileName,
				"mime_type":       item.mimeType,
				"width":           item.width,
				"height":          item.height,
				"source":          "library",
				"library_file_id": item.libraryFileID,
				"is_big_paste":    false,
			})
		}
		parts = append(parts, prompt)
		message["content"] = map[string]any{"content_type": "multimodal_text", "parts": parts}
		message["metadata"] = map[string]any{
			"attachments":                  attachments,
			"developer_mode_connector_ids": []any{},
			"selected_sources":             []any{},
			"selected_github_repos":        []any{},
			"selected_all_github_repos":    false,
			"serialization_metadata":       map[string]any{"custom_symbol_offsets": []any{}},
		}
	} else {
		message["content"] = map[string]any{"content_type": "text", "parts": []any{prompt}}
	}

	payload := map[string]any{
		"action":                   "next",
		"messages":                 []any{message},
		"parent_message_id":        "client-created-root",
		"model":                    protocol.EditableFileModel,
		"client_prepare_state":     "sent",
		"timezone_offset_min":      -480,
		"timezone":                 "Asia/Shanghai",
		"conversation_mode":        map[string]any{"kind": "primary_assistant"},
		"enable_message_followups": true,
		"system_hints":             []any{},
		"supports_buffering":       true,
		"supported_encodings":      []any{"v1"},
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": 401,
			"page_height":       1138,
			"page_width":        803,
			"pixel_ratio":       2,
			"screen_height":     1440,
			"screen_width":      2560,
			"app_name":          "chatgpt.com",
		},
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
		"thinking_effort":                      editableThinkingEffort,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	path := "/backend-api/f/conversation"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+path, newBytesReader(body))
	if err != nil {
		return "", err
	}
	req.Header = c.imageHeaders(path, requirements, conduitToken, "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return "", NewStatusError(resp.StatusCode, retryAfterFromBody(preview),
			fmt.Sprintf("%s failed: status=%d body=%s", path, resp.StatusCode, truncate(string(preview), 300)))
	}

	payloads := make(chan string, 64)
	errs := make(chan error, 1)
	go func() {
		defer close(payloads)
		defer close(errs)
		defer resp.Body.Close()
		if readErr := readSSEPayloads(resp.Body, payloads); readErr != nil {
			errs <- readErr
		}
	}()

	conversationID := ""
	for ssePayload := range payloads {
		if conversationID == "" {
			conversationID = protocol.FindSearchValue(ssePayload, "conversation_id")
		}
	}
	var readErr error
	for err := range errs {
		readErr = err
	}
	if readErr != nil {
		return "", readErr
	}
	if conversationID == "" {
		return "", errors.New("conversation_id not found in stream")
	}
	return conversationID, nil
}

// editableToleratedStatuses 复刻 _wait_editable_output_artifacts 里容忍的状态码集合。
//
// 会话刚建好时上游会短暂返回这些状态（文档还没落库、沙箱还在跑），
// 直接冒泡会让任务随机失败；401/403 是真错误，必须立刻抛出。
var editableToleratedStatuses = map[int]bool{
	404: true,
	409: true,
	423: true,
	429: true,
	500: true,
	502: true,
	503: true,
	504: true,
}

// waitEditableOutputArtifacts 复刻 _wait_editable_output_artifacts（:1451-1480）：
// 轮询会话文档，直到同时出现主产物与 zip。
func (c *Client) waitEditableOutputArtifacts(
	ctx context.Context,
	conversationID string,
	spec editableKindSpec,
	options editableOptions,
) ([]editableArtifact, error) {
	timeout := options.timeout
	if timeout <= 0 {
		timeout = editableTimeout
	}
	interval := options.pollInterval
	if interval <= 0 {
		interval = editablePollInterval
	}
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		conversation, err := c.getEditableConversationDetail(ctx, conversationID)
		if err != nil {
			if !editableToleratedStatuses[protocol.StatusCodeOf(err)] {
				return nil, err
			}
		} else {
			targeted := pickEditableTargetArtifacts(
				extractEditableArtifacts(conversation, spec.exportFileRe), spec)
			if len(targeted) > 0 {
				return targeted, nil
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
	return nil, fmt.Errorf("timed out waiting for %s/zip outputs", spec.label)
}

// getEditableConversationDetail 复刻 _get_editable_conversation_detail（:1482-1486）。
func (c *Client) getEditableConversationDetail(ctx context.Context, conversationID string) (map[string]any, error) {
	path := "/backend-api/conversation/" + conversationID
	body, status, err := c.editableBrowserGET(ctx, path,
		"/backend-api/conversation/{conversation_id}", nil, conversationID, 60*time.Second)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, NewStatusError(status, retryAfterFromBody(body),
			fmt.Sprintf("%s failed: status=%d body=%s", path, status, truncate(string(body), 300)))
	}
	var conversation map[string]any
	if err := json.Unmarshal(body, &conversation); err != nil {
		return nil, fmt.Errorf("%s: invalid json: %w", path, err)
	}
	return conversation, nil
}

// editableBrowserGET 复刻 _editable_browser_headers + 请求本身：
// Referer 指向 /c/{id}，X-OpenAI-Target-Route 用调用方给的**字面量**路由。
func (c *Client) editableBrowserGET(
	ctx context.Context,
	path, route string,
	query url.Values,
	conversationID string,
	timeout time.Duration,
) ([]byte, int, error) {
	target := BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	header := c.headers(path, map[string]string{
		"Accept":                "*/*",
		"Referer":               BaseURL + "/c/" + conversationID,
		"X-OpenAI-Target-Route": route,
	})
	return c.doWithRawHeaders(ctx, http.MethodGet, target, header, nil, timeout)
}

// extractEditableArtifacts 复刻 _extract_editable_artifacts（:1498-1515）。
func extractEditableArtifacts(conversation map[string]any, exportFileRe *regexp.Regexp) []editableArtifact {
	byKey := map[string]editableArtifact{}

	mapping, _ := conversation["mapping"].(map[string]any)
	nodes := make([]map[string]any, 0, len(mapping))
	for _, raw := range mapping {
		if node, ok := raw.(map[string]any); ok {
			nodes = append(nodes, node)
		}
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		return editableMessageCreateTime(nodes[i]) < editableMessageCreateTime(nodes[j])
	})

	for _, node := range nodes {
		message, _ := node["message"].(map[string]any)
		if message == nil {
			continue
		}
		authorRole := strings.TrimSpace(stringOfValue(nestedValue(message, "author", "role")))
		// 只认 assistant / tool：用户消息里的附件是输入，不是产物。
		if authorRole != "assistant" && authorRole != "tool" {
			continue
		}
		messageID := stringOfValue(message["id"])
		createTime := numberValue(message["create_time"])
		messageText := editableMessageText(message)

		for _, artifact := range editableMessageArtifacts(message, messageID, authorRole, createTime, exportFileRe) {
			key := artifact.attachmentID
			if key == "" {
				key = artifact.fileID
			}
			if key == "" {
				key = artifact.name
			}
			if key == "" {
				key = artifact.sandboxPath
			}
			if key == "" {
				continue
			}
			byKey[key] = mergeEditableArtifact(byKey[key], artifact)
		}

		for _, exportPath := range editableExportPaths(messageText, exportFileRe) {
			inferred := editableArtifact{
				name:        path.Base(exportPath),
				createTime:  createTime,
				authorRole:  authorRole,
				sandboxPath: exportPath,
				messageID:   messageID,
			}
			byKey[exportPath] = mergeEditableArtifact(byKey[exportPath], inferred)
		}
	}

	out := make([]editableArtifact, 0, len(byKey))
	for _, artifact := range byKey {
		out = append(out, artifact)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].createTime < out[j].createTime })
	return out
}

func editableMessageCreateTime(node map[string]any) float64 {
	message, _ := node["message"].(map[string]any)
	if message == nil {
		return 0
	}
	return numberValue(message["create_time"])
}

// editableMessageArtifacts 复刻 _extract_editable_message_artifacts（:1517-1534）：
// metadata.attachments + 消息里所有嵌套 dict。
func editableMessageArtifacts(
	message map[string]any,
	messageID, authorRole string,
	createTime float64,
	exportFileRe *regexp.Regexp,
) []editableArtifact {
	out := make([]editableArtifact, 0, 2)
	if metadata, ok := message["metadata"].(map[string]any); ok {
		if attachments, ok := metadata["attachments"].([]any); ok {
			for _, raw := range attachments {
				item, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if artifact, ok := editableArtifactFromDict(item, messageID, authorRole, createTime, exportFileRe); ok {
					out = append(out, artifact)
				}
			}
		}
	}
	for _, item := range walkNestedDicts(message) {
		if artifact, ok := editableArtifactFromDict(item, messageID, authorRole, createTime, exportFileRe); ok {
			out = append(out, artifact)
		}
	}
	return out
}

// editableArtifactKeys 是「看起来像文件对象」的键集合（复刻 :1544 的交集判断）。
var editableArtifactKeys = []string{
	"id", "file_id", "asset_pointer", "name", "file_name", "filename", "mime_type", "mimeType",
}

// editableArtifactFromDict 复刻 _editable_artifact_from_dict（:1536-1567）。
func editableArtifactFromDict(
	payload map[string]any,
	messageID, authorRole string,
	createTime float64,
	exportFileRe *regexp.Regexp,
) (editableArtifact, bool) {
	looksLikeFile := false
	for _, key := range editableArtifactKeys {
		if _, ok := payload[key]; ok {
			looksLikeFile = true
			break
		}
	}
	if !looksLikeFile {
		return editableArtifact{}, false
	}

	attachmentID := editableMatchFileID(stringOfValue(payload["id"]))
	fileID := editableMatchFileID(stringOfValue(payload["file_id"]))
	name := editableSanitizeFilename(firstNonEmptyString(
		payload["name"], payload["file_name"], payload["filename"], payload["title"]))
	mimeType := cleanEditableMimeType(firstNonEmptyValue(payload["mime_type"], payload["mimeType"]))
	for _, assetID := range editableAssetPointerRe.FindAllStringSubmatch(stringOfValue(payload["asset_pointer"]), -1) {
		if attachmentID == "" {
			attachmentID = assetID[1]
		}
		if fileID == "" {
			fileID = assetID[1]
		}
	}
	if attachmentID == "" || fileID == "" {
		if dumped, err := json.Marshal(payload); err == nil {
			ids := editableExtractFileIDs(string(dumped))
			if len(ids) > 0 {
				if attachmentID == "" {
					attachmentID = ids[0]
				}
				if fileID == "" {
					fileID = ids[0]
				}
			}
		}
	}
	if attachmentID == "" && fileID == "" {
		return editableArtifact{}, false
	}

	sandboxPath := ""
	if paths := editableExportPaths(payload, exportFileRe); len(paths) > 0 {
		sandboxPath = paths[0]
	}
	return editableArtifact{
		attachmentID: attachmentID,
		fileID:       fileID,
		name:         name,
		mimeType:     mimeType,
		createTime:   createTime,
		authorRole:   authorRole,
		sandboxPath:  sandboxPath,
		messageID:    messageID,
	}, true
}

// pickEditableTargetArtifacts 复刻 _pick_editable_target_artifacts（:1569-1578）：
// 主产物与 zip 各取「最后一个」匹配项，两者都在才算成功。
func pickEditableTargetArtifacts(artifacts []editableArtifact, spec editableKindSpec) []editableArtifact {
	primary := editableLastMatch(artifacts, func(item editableArtifact) bool {
		return editableLooksLikePrimary(item, spec)
	})
	zipItem := editableLastMatch(artifacts, editableLooksLikeZip)
	if primary == nil || zipItem == nil {
		return nil
	}
	return []editableArtifact{*primary, *zipItem}
}

func editableLastMatch(artifacts []editableArtifact, match func(editableArtifact) bool) *editableArtifact {
	for index := len(artifacts) - 1; index >= 0; index-- {
		if match(artifacts[index]) {
			item := artifacts[index]
			return &item
		}
	}
	return nil
}

// editableLooksLikePrimary 复刻 _looks_like_editable_primary（:1706-1716）。
func editableLooksLikePrimary(artifact editableArtifact, spec editableKindSpec) bool {
	name := strings.ToLower(artifact.name)
	sandboxPath := strings.ToLower(artifact.sandboxPath)
	mimeType := artifact.mimeType
	if editableHasSuffix(name, spec.suffixes) || editableHasSuffix(sandboxPath, spec.suffixes) {
		return true
	}
	if spec.mimeTypes[mimeType] {
		return true
	}
	for _, keyword := range spec.mimeKeywords {
		if strings.Contains(mimeType, keyword) {
			return true
		}
	}
	return false
}

// editableLooksLikeZip 复刻 _looks_like_editable_zip（:1718-1721）。
func editableLooksLikeZip(artifact editableArtifact) bool {
	name := strings.ToLower(artifact.name)
	sandboxPath := strings.ToLower(artifact.sandboxPath)
	mimeType := artifact.mimeType
	if strings.HasSuffix(name, ".zip") || strings.HasSuffix(sandboxPath, ".zip") {
		return true
	}
	return editableZipMimeTypes[mimeType] || strings.HasSuffix(mimeType, "/zip")
}

func editableHasSuffix(value string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}

// downloadEditableArtifact 复刻 _download_editable_artifact（:1580-1592）。
func (c *Client) downloadEditableArtifact(
	ctx context.Context,
	conversationID string,
	artifact editableArtifact,
	outputDir string,
	spec editableKindSpec,
) (string, error) {
	downloadURL, err := c.resolveEditableDownloadURL(ctx, conversationID, artifact)
	if err != nil {
		return "", err
	}
	if downloadURL == "" {
		return "", fmt.Errorf("download url not found for artifact: %s", editableArtifactLabel(artifact))
	}

	// 下载产物用裸 session 语义（带 Authorization，但不带 target-path/route）：
	// 产物在 estuary/blob 上，带上 API 路由头会被当成 API 调用处理。
	header := http.Header{}
	header.Set("Accept", "*/*")
	header.Set("Referer", BaseURL+"/")
	if strings.TrimSpace(c.accessToken) != "" {
		header.Set("Authorization", "Bearer "+c.accessToken)
	}
	header.Set("User-Agent", c.userAgent)

	payload, status, err := c.doWithRawHeaders(ctx, http.MethodGet, downloadURL, header, nil, 300*time.Second)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", NewStatusError(status, nil,
			fmt.Sprintf("artifact_download failed: status=%d", status))
	}

	contentType := cleanEditableMimeType(artifact.mimeType)
	fileName := resolveEditableOutputName(artifact, downloadURL, "", contentType, spec)
	targetPath := uniqueEditablePath(filepath.Join(outputDir, fileName))
	if err := os.WriteFile(targetPath, payload, 0o644); err != nil {
		return "", err
	}
	return targetPath, nil
}

func editableArtifactLabel(artifact editableArtifact) string {
	return fmt.Sprintf("{name:%s file_id:%s sandbox_path:%s}", artifact.name, artifact.fileID, artifact.sandboxPath)
}

// resolveEditableDownloadURL 复刻 _resolve_editable_download_url（:1594-1655）：
// 依次尝试 interpreter 下载、attachment 下载、files 下载三条路径。
func (c *Client) resolveEditableDownloadURL(
	ctx context.Context,
	conversationID string,
	artifact editableArtifact,
) (string, error) {
	ids := make([]string, 0, 2)
	for _, candidate := range []string{artifact.attachmentID, artifact.fileID} {
		if candidate == "" {
			continue
		}
		duplicate := false
		for _, existing := range ids {
			if existing == candidate {
				duplicate = true
				break
			}
		}
		if !duplicate {
			ids = append(ids, candidate)
		}
	}

	// 1) 沙箱路径下载（interpreter 产物走这条）。
	if artifact.sandboxPath != "" && artifact.messageID != "" {
		path := "/backend-api/conversation/" + conversationID + "/interpreter/download"
		query := url.Values{}
		query.Set("message_id", artifact.messageID)
		query.Set("sandbox_path", artifact.sandboxPath)
		if resolved, err := c.tryEditableDownload(ctx, path,
			"/backend-api/conversation/{conversation_id}/interpreter/download", query, conversationID); err != nil {
			return "", err
		} else if resolved != "" {
			return resolved, nil
		}
	}

	// 2) attachment 下载。
	for _, id := range ids {
		path := "/backend-api/conversation/" + conversationID + "/attachment/" + id + "/download"
		if resolved, err := c.tryEditableDownload(ctx, path,
			"/backend-api/conversation/{conversation_id}/attachment/{attachment_id}/download", nil, conversationID); err != nil {
			return "", err
		} else if resolved != "" {
			return resolved, nil
		}
	}

	// 3) files/download/{id}（带 post_id / inline 参数）。
	for _, id := range ids {
		path := "/backend-api/files/download/" + id
		query := url.Values{}
		query.Set("post_id", "")
		query.Set("inline", "false")
		if resolved, err := c.tryEditableDownload(ctx, path,
			"/backend-api/files/download/{file_id}", query, conversationID); err != nil {
			return "", err
		} else if resolved != "" {
			return resolved, nil
		}
	}

	// 4) files/{id}/download（老形状，Python 也保留着）。
	for _, id := range ids {
		path := "/backend-api/files/" + id + "/download"
		if resolved, err := c.tryEditableDownload(ctx, path,
			"/backend-api/files/download/{file_id}", nil, conversationID); err != nil {
			return "", err
		} else if resolved != "" {
			return resolved, nil
		}
	}
	return "", nil
}

// tryEditableDownload 发一次下载解析请求：2xx 且拿到 url 才算命中。
//
// 非 2xx 一律当「这条路径不适用」继续下一条（Python 只在 2xx 时取 url），
// 只有网络层错误才冒泡。
func (c *Client) tryEditableDownload(
	ctx context.Context,
	path, route string,
	query url.Values,
	conversationID string,
) (string, error) {
	body, status, err := c.editableBrowserGET(ctx, path, route, query, conversationID, 60*time.Second)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", nil
	}
	if url := stringOfValue(payload["download_url"]); url != "" {
		return url, nil
	}
	return stringOfValue(payload["url"]), nil
}

// editableMimeExtensions 是 mimetypes.guess_extension 在产物 MIME 上的结果。
var editableMimeExtensions = map[string]string{
	"application/pdf":              ".pdf",
	"image/png":                    ".png",
	"image/jpeg":                   ".jpg",
	"image/webp":                   ".webp",
	"application/json":             ".json",
	"text/plain":                   ".txt",
	"text/csv":                     ".csv",
	"text/html":                    ".html",
	"application/zip":              ".zip",
	"application/x-zip-compressed": ".zip",
}

// resolveEditableOutputName 复刻 _resolve_editable_output_name（:1665-1687）：
// 名字优先级 artifact.name → sandbox 文件名 → Content-Disposition → URL 文件名，
// 没有扩展名时按 MIME 补。
func resolveEditableOutputName(
	artifact editableArtifact,
	finalURL, contentDisposition, contentType string,
	spec editableKindSpec,
) string {
	fileName := editableSanitizeFilename(artifact.name)
	if fileName == "" && artifact.sandboxPath != "" {
		fileName = editableSanitizeFilename(path.Base(artifact.sandboxPath))
	}
	if fileName == "" {
		fileName = editableSanitizeFilename(editableFilenameFromContentDisposition(contentDisposition))
	}
	if fileName == "" {
		if parsed, err := url.Parse(finalURL); err == nil {
			fileName = editableSanitizeFilename(path.Base(parsed.Path))
		}
	}
	extension := editableExtensionFromMimeType(contentType, spec)
	if fileName == "" {
		return "artifact" + extension
	}
	if filepath.Ext(fileName) == "" || filepath.Ext(fileName) == "." {
		return fileName + extension
	}
	return fileName
}

// editableExtensionFromMimeType 复刻 _editable_extension_from_mime_type（:1723-1735）。
func editableExtensionFromMimeType(mimeType string, spec editableKindSpec) string {
	if spec.mimeTypes[mimeType] {
		return spec.defaultExtension
	}
	for _, keyword := range spec.mimeKeywords {
		if strings.Contains(mimeType, keyword) {
			return spec.defaultExtension
		}
	}
	if editableZipMimeTypes[mimeType] || strings.HasSuffix(mimeType, "/zip") {
		return ".zip"
	}
	return editableMimeExtensions[mimeType]
}

// editableFilenameFromContentDisposition 复刻
// _editable_filename_from_content_disposition（:1737-1744）。
func editableFilenameFromContentDisposition(contentDisposition string) string {
	if contentDisposition == "" {
		return ""
	}
	if match := editableContentDispositionExtendedRe.FindStringSubmatch(contentDisposition); match != nil {
		if decoded, err := url.PathUnescape(match[1]); err == nil {
			return strings.TrimSpace(decoded)
		}
		return strings.TrimSpace(match[1])
	}
	if match := editableContentDispositionPlainRe.FindStringSubmatch(contentDisposition); match != nil {
		return strings.TrimSpace(match[1])
	}
	return ""
}

var (
	editableContentDispositionExtendedRe = regexp.MustCompile(`(?i)filename\*=UTF-8''([^;]+)`)
	editableContentDispositionPlainRe    = regexp.MustCompile(`(?i)filename="([^"]+)"`)
)

// editableSanitizeFilename 复刻 _sanitize_editable_filename（:1746-1748）：
// 只保留 basename 并去掉 NUL。
func editableSanitizeFilename(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	base := path.Base(strings.ReplaceAll(trimmed, "\x00", ""))
	if base == "." || base == "/" {
		return ""
	}
	return strings.TrimSpace(base)
}

// uniqueEditablePath 复刻 _unique_editable_path（:1750-1758）。
func uniqueEditablePath(target string) string {
	if _, err := os.Stat(target); err != nil {
		return target
	}
	extension := filepath.Ext(target)
	stem := strings.TrimSuffix(target, extension)
	for index := 1; index < 1000; index++ {
		candidate := fmt.Sprintf("%s_%d%s", stem, index, extension)
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
	}
	return target
}

// mergeEditableArtifact 复刻 _merge_editable_artifact（:1760-1772）：
// 非空字段覆盖，create_time 取较大值。
func mergeEditableArtifact(current, latest editableArtifact) editableArtifact {
	if current.attachmentID == "" && current.fileID == "" && current.name == "" &&
		current.sandboxPath == "" && current.createTime == 0 {
		return latest
	}
	merged := current
	if latest.attachmentID != "" {
		merged.attachmentID = latest.attachmentID
	}
	if latest.fileID != "" {
		merged.fileID = latest.fileID
	}
	if latest.name != "" {
		merged.name = latest.name
	}
	if latest.mimeType != "" {
		merged.mimeType = latest.mimeType
	}
	if latest.createTime > merged.createTime {
		merged.createTime = latest.createTime
	}
	if latest.authorRole != "" {
		merged.authorRole = latest.authorRole
	}
	if latest.sandboxPath != "" {
		merged.sandboxPath = latest.sandboxPath
	}
	if latest.messageID != "" {
		merged.messageID = latest.messageID
	}
	return merged
}

// editableMessageText 复刻 _editable_message_text（:1774-1790）。
func editableMessageText(message map[string]any) string {
	if message == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	if content, ok := message["content"].(map[string]any); ok {
		if text, ok := content["text"].(string); ok {
			parts = append(parts, text)
		}
		if rawParts, ok := content["parts"].([]any); ok {
			for _, part := range rawParts {
				switch typed := part.(type) {
				case string:
					parts = append(parts, typed)
				case map[string]any:
					for _, key := range []string{"text", "asset_pointer", "model_set_context"} {
						if value := stringOfValue(typed[key]); value != "" {
							parts = append(parts, value)
						}
					}
				}
			}
		}
	}
	if text, ok := message["content"].(string); ok {
		parts = append(parts, text)
	}
	nonEmpty := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			nonEmpty = append(nonEmpty, part)
		}
	}
	return strings.TrimSpace(strings.Join(nonEmpty, "\n"))
}

// editableExportPaths 复刻 _extract_editable_export_paths（:1792-1806）。
func editableExportPaths(payload any, exportFileRe *regexp.Regexp) []string {
	text := ""
	if raw, ok := payload.(string); ok {
		text = raw
	} else if dumped, err := json.Marshal(payload); err == nil {
		text = string(dumped)
	} else {
		text = fmt.Sprintf("%v", payload)
	}
	values := make([]string, 0, 2)
	for _, match := range exportFileRe.FindAllStringSubmatch(text, -1) {
		candidate := strings.TrimSpace(match[1])
		if candidate == "" {
			continue
		}
		duplicate := false
		for _, existing := range values {
			if existing == candidate {
				duplicate = true
				break
			}
		}
		if !duplicate {
			values = append(values, candidate)
		}
	}
	return values
}

// editableExtractFileIDs 复刻 _extract_editable_file_ids（:1689-1697）：
// 先取 asset pointer 里的 id，再取 file-xxx / file_xxx。
func editableExtractFileIDs(text string) []string {
	values := make([]string, 0, 2)
	for _, match := range editableAssetPointerRe.FindAllStringSubmatch(text, -1) {
		if !containsString(values, match[1]) {
			values = append(values, match[1])
		}
	}
	for _, match := range editableFileIDRe.FindAllStringSubmatch(text, -1) {
		if editableIsServicePlaceholder(match[1]) {
			continue
		}
		if !containsString(values, match[1]) {
			values = append(values, match[1])
		}
	}
	return values
}

// editableMatchFileID 复刻 _match_editable_file_id（:1699-1702）。
func editableMatchFileID(value string) string {
	match := editableFileIDRe.FindStringSubmatch(value)
	if match == nil || editableIsServicePlaceholder(match[1]) {
		return ""
	}
	return match[1]
}

// editableIsServicePlaceholder 补上 RE2 缺失的 (?!service\b) 负向前瞻。
func editableIsServicePlaceholder(value string) bool {
	return value == "file-service" || value == "file_service"
}

// cleanEditableMimeType 复刻 _clean_editable_mime_type（:1704-1707）：
// 小写、去参数；不含 "/" 视为空。
func cleanEditableMimeType(value any) string {
	text := strings.ToLower(strings.TrimSpace(stringOfValue(value)))
	if !strings.Contains(text, "/") {
		return ""
	}
	if index := strings.Index(text, ";"); index >= 0 {
		return text[:index]
	}
	return text
}

// walkNestedDicts 复刻 _walk_search_dicts（:2018-2023）：dict 自己 + 所有嵌套 dict。
func walkNestedDicts(payload any) []map[string]any {
	switch typed := payload.(type) {
	case map[string]any:
		out := []map[string]any{typed}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			out = append(out, walkNestedDicts(typed[key])...)
		}
		return out
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, walkNestedDicts(item)...)
		}
		return out
	default:
		return nil
	}
}

func nestedValue(payload map[string]any, keys ...string) any {
	var current any = payload
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

func firstNonEmptyValue(values ...any) any {
	for _, value := range values {
		if value == nil {
			continue
		}
		if text, ok := value.(string); ok {
			if strings.TrimSpace(text) == "" {
				continue
			}
			return text
		}
		return value
	}
	return nil
}

func firstNonEmptyString(values ...any) string {
	return stringOfValue(firstNonEmptyValue(values...))
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// doWithHeaders 用指纹头发请求（extra 覆盖默认值）。
func (c *Client) doWithHeaders(
	ctx context.Context,
	method, target, path string,
	extra map[string]string,
	body []byte,
	timeout time.Duration,
) ([]byte, int, error) {
	return c.doWithRawHeaders(ctx, method, target, c.headers(path, extra), body, timeout)
}

// doWithRawHeaders 是带显式头的请求出口（PUT blob、产物下载用）。
func (c *Client) doWithRawHeaders(
	ctx context.Context,
	method, target string,
	header http.Header,
	body []byte,
	timeout time.Duration,
) ([]byte, int, error) {
	requestCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var reader io.Reader
	if body != nil {
		reader = newBytesReader(body)
	}
	req, err := http.NewRequestWithContext(requestCtx, method, target, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header = header

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return payload, resp.StatusCode, nil
}
