package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"

	"github.com/basketikun/chatgpt2api/internal/protocol"
)

// editableFake 是可编辑文件链路的可编程上游：按路径（必要时按方法）回放响应，
// 并记录每次请求的方法/路径/头/体。
//
// 与 searchFake 的差别：这条链路多了 PUT blob 上传、interpreter 下载解析、
// 产物字节下载，所以按路径分段回放，并把 conversation 文档做成脚本序列
// （第一次没有产物、第二次才有 —— 模拟真实的轮询推进）。
type editableFake struct {
	tls_client.HttpClient

	requests []*editableFakeRequest

	bootstrap            captureResponse
	requirementsPrepare  captureResponse
	requirementsFinalize captureResponse
	prepare              captureResponse
	stream               captureResponse
	fileCreate           captureResponse
	blobPut              captureResponse
	uploadFinished       captureResponse

	// conversationResponses 按调用次数依次取，最后一条重复。
	conversationResponses []captureResponse
	conversationCalls     int

	// downloadResolutions 是 interpreter/attachment/files 解析端点的响应。
	downloadResolution captureResponse
	// artifactBytes 是产物下载（GET 产物 URL）返回的字节。
	artifactBytes map[string]string
	artifactTypes map[string]string
}

// editableFakeRequest 是一次被记录的出站请求。
type editableFakeRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

func (f *editableFake) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	f.requests = append(f.requests, &editableFakeRequest{
		method: req.Method,
		path:   req.URL.Path,
		header: req.Header,
		body:   body,
	})

	switch {
	case req.URL.Path == "/":
		return editableResponse(f.bootstrap), nil
	case req.URL.Path == "/backend-api/sentinel/chat-requirements/prepare":
		return editableResponse(f.requirementsPrepare), nil
	case req.URL.Path == "/backend-api/sentinel/chat-requirements/finalize":
		return editableResponse(f.requirementsFinalize), nil
	case req.URL.Path == "/backend-api/f/conversation/prepare":
		return editableResponse(f.prepare), nil
	case req.URL.Path == "/backend-api/f/conversation":
		return editableResponse(f.stream), nil
	case req.URL.Path == "/backend-api/files" && req.Method == http.MethodPost:
		return editableResponse(f.fileCreate), nil
	case strings.HasPrefix(req.URL.Path, "/blob/"):
		return editableResponse(f.blobPut), nil
	case strings.HasPrefix(req.URL.Path, "/backend-api/files/") && strings.HasSuffix(req.URL.Path, "/uploaded"):
		return editableResponse(f.uploadFinished), nil
	case strings.Contains(req.URL.Path, "/interpreter/download"),
		strings.Contains(req.URL.Path, "/attachment/"),
		strings.HasPrefix(req.URL.Path, "/backend-api/files/download/"),
		(strings.HasPrefix(req.URL.Path, "/backend-api/files/") && strings.HasSuffix(req.URL.Path, "/download")):
		return editableResponse(f.downloadResolution), nil
	case strings.HasPrefix(req.URL.Path, "/artifact/"):
		name := strings.TrimPrefix(req.URL.Path, "/artifact/")
		contentType := f.artifactTypes[name]
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		return editableResponse(captureResponse{
			status:      200,
			contentType: contentType,
			body:        f.artifactBytes[name],
		}), nil
	case strings.HasPrefix(req.URL.Path, "/backend-api/conversation/"):
		return editableResponse(f.nextConversation()), nil
	default:
		return editableResponse(captureResponse{status: 404, body: `{"detail":"not found"}`}), nil
	}
}

// nextConversation 取下一条会话文档脚本。
func (f *editableFake) nextConversation() captureResponse {
	if len(f.conversationResponses) == 0 {
		return captureResponse{status: 500, body: `{"detail":"no conversation scripted"}`}
	}
	index := f.conversationCalls
	if index >= len(f.conversationResponses) {
		index = len(f.conversationResponses) - 1
	}
	f.conversationCalls++
	return f.conversationResponses[index]
}

func editableResponse(response captureResponse) *http.Response {
	if response.status == 0 {
		response.status = 200
	}
	header := http.Header{}
	if response.contentType != "" {
		header["content-type"] = []string{response.contentType}
	}
	return &http.Response{
		StatusCode: response.status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(response.body)),
	}
}

// requestsFor 返回某路径上的全部请求。
func (f *editableFake) requestsFor(path string) []*editableFakeRequest {
	out := make([]*editableFakeRequest, 0, 1)
	for _, request := range f.requests {
		if request.path == path {
			out = append(out, request)
		}
	}
	return out
}

// requireSingleRequest 断言某路径只被请求一次并返回它。
func (f *editableFake) requireSingleRequest(t *testing.T, path string) *editableFakeRequest {
	t.Helper()
	requests := f.requestsFor(path)
	if len(requests) != 1 {
		t.Fatalf("%s request count = %d, want 1", path, len(requests))
	}
	return requests[0]
}

// newEditableFake 构造一个绑定假上游的 Client（带一份「一次成功」的默认脚本）。
func newEditableFake(t *testing.T, accessToken string) (*Client, *editableFake) {
	t.Helper()
	fake := &editableFake{
		bootstrap: captureResponse{
			contentType: "text/html",
			body:        `<html data-build="build-1"><script src="/backend-api/sentinel/sdk.js"></script></html>`,
		},
		requirementsPrepare:  captureResponse{body: `{"prepare_token":"pt-1"}`},
		requirementsFinalize: captureResponse{body: `{"token":"sentinel-token"}`},
		prepare:              captureResponse{body: `{"conduit_token":"conduit-1"}`},
		stream: captureResponse{
			contentType: "text/event-stream",
			body:        "data: {\"conversation_id\":\"cid-1\"}\n\ndata: [DONE]\n\n",
		},
		fileCreate: captureResponse{
			contentType: "application/json",
			body:        `{"upload_url":"https://blob.example/blob/image_1","file_id":"file-upload1","library_file_id":"lib-1"}`,
		},
		blobPut:        captureResponse{status: 201, body: ``},
		uploadFinished: captureResponse{contentType: "application/json", body: `{}`},
		downloadResolution: captureResponse{
			contentType: "application/json",
			body:        `{"download_url":"https://cdn.example/artifact/PLACEHOLDER"}`,
		},
		artifactBytes: map[string]string{},
		artifactTypes: map[string]string{},
	}
	client := &Client{
		http:        fake,
		profile:     defaultProfile,
		accessToken: accessToken,
		userAgent:   "test-agent",
		deviceID:    "device",
		sessionID:   "session",
	}
	return client, fake
}

// fastEditableOptions 压短超时与轮询间隔，避免测试真的等 5 秒。
func fastEditableOptions() editableOptions {
	return editableOptions{timeout: 400 * time.Millisecond, pollInterval: time.Millisecond}
}

// editableConversation 造一份会话文档：一个 assistant 消息，带给定附件。
func editableConversation(t *testing.T, attachments []map[string]any, messageText string) string {
	t.Helper()
	parts := []any{}
	if messageText != "" {
		parts = append(parts, messageText)
	}
	message := map[string]any{
		"id":          "m-1",
		"author":      map[string]any{"role": "assistant"},
		"create_time": 100.0,
		"content":     map[string]any{"content_type": "text", "parts": parts},
		"metadata":    map[string]any{"attachments": attachments},
	}
	payload, err := json.Marshal(map[string]any{
		"mapping": map[string]any{"node-1": map[string]any{"message": message}},
	})
	if err != nil {
		t.Fatalf("marshal conversation: %v", err)
	}
	return string(payload)
}

// editableAttachment 造一条附件元数据。
func editableAttachment(id, name, mimeType string) map[string]any {
	return map[string]any{
		"id":        id,
		"file_id":   id,
		"name":      name,
		"mime_type": mimeType,
	}
}

// 两个固定 prompt 模板必须与 Python 逐字一致。
//
// 用 sha256 而不是「片段包含」：这两个模板是长中文串，手抄错一个字
// （或把全角逗号写成半角）都会让上游模型误解任务，而片段断言看不出来。
func TestEditablePromptTemplatesMatchPythonVerbatim(t *testing.T) {
	cases := []struct {
		name     string
		prompt   string
		length   int
		checksum string
	}{
		{
			name:     "ppt",
			prompt:   editablePPTDefaultPrompt,
			length:   309,
			checksum: "83ff395ba8d25341251ba498c5f081adc9ba6bc2993fc7b5103f494f47598460",
		},
		{
			name:     "psd",
			prompt:   editablePSDDefaultPrompt,
			length:   163,
			checksum: "7e32b320d1970b0757b78d00a03a2b8d5a48a613cd0926f4af7b21f53dc19ec1",
		},
	}
	for _, item := range cases {
		if len([]rune(item.prompt)) != item.length {
			t.Errorf("%s prompt rune length = %d, want %d", item.name, len([]rune(item.prompt)), item.length)
		}
		sum := sha256.Sum256([]byte(item.prompt))
		if got := hex.EncodeToString(sum[:]); got != item.checksum {
			t.Errorf("%s prompt sha256 = %s, want %s", item.name, got, item.checksum)
		}
	}

	// PPT 模板是多行模板（4 个换行），PSD 是单行 —— 复制时最容易丢掉这个差别。
	if strings.Count(editablePPTDefaultPrompt, "\n") != 4 {
		t.Errorf("ppt prompt newlines = %d, want 4", strings.Count(editablePPTDefaultPrompt, "\n"))
	}
	if strings.Contains(editablePSDDefaultPrompt, "\n") {
		t.Error("psd prompt must be a single line")
	}
	// 首尾不能有空白（Python 的 """...""" 直接从「我」开始、「。」结束）。
	if strings.HasPrefix(editablePPTDefaultPrompt, "\n") || strings.HasSuffix(editablePPTDefaultPrompt, "\n") {
		t.Error("ppt prompt must not start or end with a newline")
	}
}

// 用户补充需求的拼装规则（_editable_prompt）。
func TestEditablePromptAppendsUserRequirement(t *testing.T) {
	if got := editablePrompt("FIXED", "  "); got != "FIXED" {
		t.Errorf("empty user prompt should keep template only, got %q", got)
	}
	want := "FIXED\n\n以下是用户补充需求，请直接结合执行：\n多加一页"
	if got := editablePrompt("FIXED", " 多加一页 "); got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}
}

// 上传流程：POST /backend-api/files → PUT upload_url → POST .../uploaded。
func TestEditableUploadFlow(t *testing.T) {
	client, fake := newEditableFake(t, "token-1")
	uploaded, err := client.uploadEditableBase64Image(context.Background(), "data:image/png;base64,"+tinyPNGBase64, 1)
	if err != nil {
		t.Fatalf("uploadEditableBase64Image: %v", err)
	}
	if uploaded.fileID != "file-upload1" || uploaded.libraryFileID != "lib-1" {
		t.Fatalf("uploaded = %+v, want file-upload1 / lib-1", uploaded)
	}
	if uploaded.fileName != "image_1.png" {
		t.Errorf("file_name = %q, want image_1.png", uploaded.fileName)
	}
	if uploaded.mimeType != "image/png" {
		t.Errorf("mime_type = %q, want image/png", uploaded.mimeType)
	}
	if uploaded.width != 1 || uploaded.height != 1 {
		t.Errorf("dimensions = %dx%d, want 1x1", uploaded.width, uploaded.height)
	}

	create := fake.requireSingleRequest(t, "/backend-api/files")
	var createBody map[string]any
	if err := json.Unmarshal(create.body, &createBody); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	for key, want := range map[string]any{
		"file_name":                "image_1.png",
		"use_case":                 "multimodal",
		"store_in_library":         true,
		"library_persistence_mode": "opportunistic",
		"reset_rate_limits":        false,
	} {
		if got := createBody[key]; got != want {
			t.Errorf("create body[%q] = %v, want %v", key, got, want)
		}
	}
	if size, _ := createBody["file_size"].(float64); int(size) != len(mustDecodeBase64(t, tinyPNGBase64)) {
		t.Errorf("create body file_size = %v, want decoded length", createBody["file_size"])
	}

	// PUT 必须打到 upload_url，且带 Azure blob 的三个关键头。
	puts := fake.requestsFor("/blob/image_1")
	if len(puts) != 1 || puts[0].method != http.MethodPut {
		t.Fatalf("blob put requests = %+v, want exactly one PUT", puts)
	}
	if got := puts[0].header.Get("x-ms-blob-type"); got != "BlockBlob" {
		t.Errorf("x-ms-blob-type = %q, want BlockBlob", got)
	}
	if got := puts[0].header.Get("content-type"); got != "image/png" {
		t.Errorf("blob content-type = %q, want image/png", got)
	}

	finish := fake.requireSingleRequest(t, "/backend-api/files/file-upload1/uploaded")
	if finish.method != http.MethodPost {
		t.Errorf("finish method = %q, want POST", finish.method)
	}
	if string(finish.body) != "{}" {
		t.Errorf("finish body = %q, want {}", string(finish.body))
	}
}

// prepare/conversation 的请求体关键字段（模型、思考强度、附件形态）。
func TestEditablePrepareAndConversationBodies(t *testing.T) {
	client, fake := newEditableFake(t, "token-1")
	fake.conversationResponses = []captureResponse{conversationResponse(editableConversation(t, []map[string]any{
		editableAttachment("file-ppt1", "deck.pptx",
			"application/vnd.openxmlformats-officedocument.presentationml.presentation"),
		editableAttachment("file-zip1", "assets.zip", "application/zip"),
	}, ""))}
	fake.downloadResolution = captureResponse{
		contentType: "application/json",
		body:        `{"download_url":"https://cdn.example/artifact/placeholder"}`,
	}
	fake.artifactBytes["placeholder"] = "PAYLOAD"
	fake.artifactTypes["placeholder"] = "application/octet-stream"

	outputDir := t.TempDir()
	_, err := client.exportEditableFile(context.Background(), protocol.EditableFileRequest{
		Kind:         protocol.EditableKindPPT,
		Prompt:       "做一份产品介绍",
		Base64Images: []string{"data:image/png;base64," + tinyPNGBase64},
		OutputDir:    outputDir,
	}, fastEditableOptions())
	if err != nil {
		t.Fatalf("exportEditableFile: %v", err)
	}

	prepare := fake.requireSingleRequest(t, "/backend-api/f/conversation/prepare")
	var prepareBody map[string]any
	if err := json.Unmarshal(prepare.body, &prepareBody); err != nil {
		t.Fatalf("decode prepare body: %v", err)
	}
	if got := prepareBody["model"]; got != "gpt-5-5-thinking" {
		t.Errorf("prepare model = %v, want gpt-5-5-thinking", got)
	}
	if got := prepareBody["thinking_effort"]; got != "extended" {
		t.Errorf("prepare thinking_effort = %v, want extended", got)
	}
	if got := prepareBody["parent_message_id"]; got != "client-created-root" {
		t.Errorf("prepare parent_message_id = %v, want client-created-root", got)
	}
	if got := headerValue(prepare.header, "X-Conduit-Token"); got != "no-token" {
		t.Errorf("prepare X-Conduit-Token = %q, want no-token", got)
	}
	// 可编辑文件链路没有 picture_v2（与图片链路的关键差异）。
	if hints, ok := prepareBody["system_hints"].([]any); !ok || len(hints) != 0 {
		t.Errorf("prepare system_hints = %v, want []", prepareBody["system_hints"])
	}
	if mimes, ok := prepareBody["attachment_mime_types"].([]any); !ok || len(mimes) != 1 || mimes[0] != "image/png" {
		t.Errorf("prepare attachment_mime_types = %v, want [image/png]", prepareBody["attachment_mime_types"])
	}

	conversation := fake.requireSingleRequest(t, "/backend-api/f/conversation")
	if got := headerValue(conversation.header, "X-Conduit-Token"); got != "conduit-1" {
		t.Errorf("conversation X-Conduit-Token = %q, want conduit-1", got)
	}
	if got := headerValue(conversation.header, "OpenAI-Sentinel-Chat-Requirements-Token"); got != "sentinel-token" {
		t.Errorf("conversation sentinel token = %q, want sentinel-token", got)
	}

	var conversationBody map[string]any
	if err := json.Unmarshal(conversation.body, &conversationBody); err != nil {
		t.Fatalf("decode conversation body: %v", err)
	}
	if got := conversationBody["model"]; got != "gpt-5-5-thinking" {
		t.Errorf("conversation model = %v, want gpt-5-5-thinking", got)
	}
	if got := conversationBody["thinking_effort"]; got != "extended" {
		t.Errorf("conversation thinking_effort = %v, want extended", got)
	}
	if got := conversationBody["client_prepare_state"]; got != "sent" {
		t.Errorf("conversation client_prepare_state = %v, want sent", got)
	}

	messages, _ := conversationBody["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %v, want exactly 1", conversationBody["messages"])
	}
	message, _ := messages[0].(map[string]any)
	content, _ := message["content"].(map[string]any)
	if got := content["content_type"]; got != "multimodal_text" {
		t.Errorf("content_type = %v, want multimodal_text", got)
	}
	parts, _ := content["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %v, want [asset, prompt]", parts)
	}
	asset, _ := parts[0].(map[string]any)
	if got := asset["asset_pointer"]; got != "sediment://file-upload1" {
		t.Errorf("asset_pointer = %v, want sediment://file-upload1", got)
	}
	if parts[1] != editablePPTDefaultPrompt+"\n\n以下是用户补充需求，请直接结合执行：\n做一份产品介绍" {
		t.Errorf("prompt part = %v, want template + user requirement", parts[1])
	}

	metadata, _ := message["metadata"].(map[string]any)
	attachments, _ := metadata["attachments"].([]any)
	if len(attachments) != 1 {
		t.Fatalf("attachments = %v, want 1", metadata["attachments"])
	}
	attachment, _ := attachments[0].(map[string]any)
	if got := attachment["source"]; got != "library" {
		t.Errorf("attachment source = %v, want library", got)
	}
	if got := attachment["library_file_id"]; got != "lib-1" {
		t.Errorf("attachment library_file_id = %v, want lib-1", got)
	}
	if got := attachment["mime_type"]; got != "image/png" {
		t.Errorf("attachment mime_type = %v, want image/png", got)
	}
}

// 轮询到 pptx + zip 才算成功，并且两个文件都要落到 output dir。
func TestEditableExportPPTDownloadsBothArtifacts(t *testing.T) {
	client, fake := newEditableFake(t, "token-1")
	// 第一轮还没有产物，第二轮才有 —— 断言轮询真的会重试。
	fake.conversationResponses = []captureResponse{
		conversationResponse(editableConversation(t, nil, "正在处理…")),
		conversationResponse(editableConversation(t, []map[string]any{
			editableAttachment("file-ppt1", "deck.pptx",
				"application/vnd.openxmlformats-officedocument.presentationml.presentation"),
			editableAttachment("file-zip1", "assets.zip", "application/zip"),
		}, "")),
	}
	fake.downloadResolution = captureResponse{
		contentType: "application/json",
		body:        `{"download_url":"https://cdn.example/artifact/placeholder"}`,
	}
	fake.artifactBytes["placeholder"] = "PAYLOAD"
	fake.artifactTypes["placeholder"] = "application/octet-stream"

	outputDir := t.TempDir()
	result, err := client.exportEditableFile(context.Background(), protocol.EditableFileRequest{
		Kind:      protocol.EditableKindPPT,
		OutputDir: outputDir,
	}, fastEditableOptions())
	if err != nil {
		t.Fatalf("exportEditableFile: %v", err)
	}
	if result.ConversationID != "cid-1" {
		t.Errorf("conversation_id = %q, want cid-1", result.ConversationID)
	}
	if filepath.Base(result.PrimaryPath) != "deck.pptx" {
		t.Errorf("primary path = %q, want .../deck.pptx", result.PrimaryPath)
	}
	if filepath.Base(result.ZipPath) != "assets.zip" {
		t.Errorf("zip path = %q, want .../assets.zip", result.ZipPath)
	}
	for _, path := range []string{result.PrimaryPath, result.ZipPath} {
		payload, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		if string(payload) != "PAYLOAD" {
			t.Errorf("%s content = %q, want PAYLOAD", path, string(payload))
		}
	}
	if fake.conversationCalls < 2 {
		t.Errorf("conversation polls = %d, want >= 2 (must retry while artifacts are missing)", fake.conversationCalls)
	}
}

// 只有主产物、没有 zip：必须报超时错误，不能返回半成品。
func TestEditableExportFailsWithoutZip(t *testing.T) {
	client, fake := newEditableFake(t, "token-1")
	fake.conversationResponses = []captureResponse{conversationResponse(editableConversation(t, []map[string]any{
		editableAttachment("file-ppt1", "deck.pptx",
			"application/vnd.openxmlformats-officedocument.presentationml.presentation"),
	}, ""))}

	_, err := client.exportEditableFile(context.Background(), protocol.EditableFileRequest{
		Kind:      protocol.EditableKindPPT,
		OutputDir: t.TempDir(),
	}, fastEditableOptions())
	if err == nil {
		t.Fatal("exportEditableFile should fail when the zip artifact is missing")
	}
	if !strings.Contains(err.Error(), "timed out waiting for ppt/zip outputs") {
		t.Errorf("error = %q, want timeout wording", err.Error())
	}
}

// 只有 zip、没有主产物：同样失败。
func TestEditableExportFailsWithoutPrimary(t *testing.T) {
	client, fake := newEditableFake(t, "token-1")
	fake.conversationResponses = []captureResponse{conversationResponse(editableConversation(t, []map[string]any{
		editableAttachment("file-zip1", "assets.zip", "application/zip"),
	}, ""))}

	_, err := client.exportEditableFile(context.Background(), protocol.EditableFileRequest{
		Kind:      protocol.EditableKindPPT,
		OutputDir: t.TempDir(),
	}, fastEditableOptions())
	if err == nil {
		t.Fatal("exportEditableFile should fail when the primary artifact is missing")
	}
	if !strings.Contains(err.Error(), "timed out waiting for ppt/zip outputs") {
		t.Errorf("error = %q, want timeout wording", err.Error())
	}
}

// PSD 无参考图：必须报 base64_images is empty（Python 同文案）。
func TestEditableExportPSDRequiresImages(t *testing.T) {
	client, fake := newEditableFake(t, "token-1")

	_, err := client.exportEditableFile(context.Background(), protocol.EditableFileRequest{
		Kind:      protocol.EditableKindPSD,
		OutputDir: t.TempDir(),
	}, fastEditableOptions())
	if err == nil || err.Error() != "base64_images is empty" {
		t.Fatalf("error = %v, want base64_images is empty", err)
	}
	if len(fake.requests) != 0 {
		t.Errorf("no upstream request should be made, got %d", len(fake.requests))
	}
}

// PSD 成功路径：psd + zip，且 prompt 用 PSD 模板。
func TestEditableExportPSDUsesPSDPromptAndArtifacts(t *testing.T) {
	client, fake := newEditableFake(t, "token-1")
	fake.conversationResponses = []captureResponse{conversationResponse(editableConversation(t, []map[string]any{
		editableAttachment("file-psd1", "poster.psd", "image/vnd.adobe.photoshop"),
		editableAttachment("file-zip1", "layers.zip", "application/x-zip-compressed"),
	}, ""))}
	fake.downloadResolution = captureResponse{
		contentType: "application/json",
		body:        `{"download_url":"https://cdn.example/artifact/placeholder"}`,
	}
	fake.artifactBytes["placeholder"] = "PSD"
	fake.artifactTypes["placeholder"] = "application/octet-stream"

	result, err := client.exportEditableFile(context.Background(), protocol.EditableFileRequest{
		Kind:         protocol.EditableKindPSD,
		Base64Images: []string{"data:image/png;base64," + tinyPNGBase64},
		OutputDir:    t.TempDir(),
	}, fastEditableOptions())
	if err != nil {
		t.Fatalf("exportEditableFile: %v", err)
	}
	if filepath.Base(result.PrimaryPath) != "poster.psd" || filepath.Base(result.ZipPath) != "layers.zip" {
		t.Fatalf("artifacts = %q / %q, want poster.psd / layers.zip", result.PrimaryPath, result.ZipPath)
	}

	conversation := fake.requireSingleRequest(t, "/backend-api/f/conversation")
	var body map[string]any
	if err := json.Unmarshal(conversation.body, &body); err != nil {
		t.Fatalf("decode conversation body: %v", err)
	}
	messages, _ := body["messages"].([]any)
	message, _ := messages[0].(map[string]any)
	content, _ := message["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) != 2 || parts[1] != editablePSDDefaultPrompt {
		t.Fatalf("psd prompt part = %v, want the PSD template verbatim", parts)
	}
}

// 沙箱路径产物（消息正文里的 /mnt/data/*.pptx）：走 interpreter 下载解析。
func TestEditableExportFindsSandboxPathsInMessageText(t *testing.T) {
	client, fake := newEditableFake(t, "token-1")
	fake.conversationResponses = []captureResponse{conversationResponse(editableConversation(t, nil,
		"已生成 sandbox:/mnt/data/deck.pptx 和 /mnt/data/assets.zip"))}
	fake.downloadResolution = captureResponse{
		contentType: "application/json",
		body:        `{"download_url":"https://cdn.example/artifact/placeholder"}`,
	}
	fake.artifactBytes["placeholder"] = "SANDBOX"
	fake.artifactTypes["placeholder"] = "application/octet-stream"

	result, err := client.exportEditableFile(context.Background(), protocol.EditableFileRequest{
		Kind:      protocol.EditableKindPPT,
		OutputDir: t.TempDir(),
	}, fastEditableOptions())
	if err != nil {
		t.Fatalf("exportEditableFile: %v", err)
	}
	if filepath.Base(result.PrimaryPath) != "deck.pptx" || filepath.Base(result.ZipPath) != "assets.zip" {
		t.Fatalf("artifacts = %q / %q, want deck.pptx / assets.zip", result.PrimaryPath, result.ZipPath)
	}

	// 沙箱路径必须打到 interpreter/download，并带 message_id + sandbox_path。
	var found bool
	for _, request := range fake.requests {
		if strings.Contains(request.path, "/interpreter/download") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("sandbox artifacts must be resolved through /interpreter/download")
	}
}

// 下载解析全部失败时必须报 download url not found。
func TestEditableExportFailsWhenDownloadURLMissing(t *testing.T) {
	client, fake := newEditableFake(t, "token-1")
	fake.conversationResponses = []captureResponse{conversationResponse(editableConversation(t, []map[string]any{
		editableAttachment("file-ppt1", "deck.pptx",
			"application/vnd.openxmlformats-officedocument.presentationml.presentation"),
		editableAttachment("file-zip1", "assets.zip", "application/zip"),
	}, ""))}
	fake.downloadResolution = captureResponse{status: 404, body: `{"detail":"not found"}`}

	_, err := client.exportEditableFile(context.Background(), protocol.EditableFileRequest{
		Kind:      protocol.EditableKindPPT,
		OutputDir: t.TempDir(),
	}, fastEditableOptions())
	if err == nil || !strings.Contains(err.Error(), "download url not found") {
		t.Fatalf("error = %v, want download url not found", err)
	}
}

// 上游 4xx（非容忍集合）必须立刻失败，而不是轮询到超时。
func TestEditableExportFailsFastOnAuthError(t *testing.T) {
	client, fake := newEditableFake(t, "token-1")
	fake.conversationResponses = []captureResponse{{status: 401, body: `{"detail":"unauthorized"}`}}

	_, err := client.exportEditableFile(context.Background(), protocol.EditableFileRequest{
		Kind:      protocol.EditableKindPPT,
		OutputDir: t.TempDir(),
	}, fastEditableOptions())
	if err == nil {
		t.Fatal("exportEditableFile should fail on 401")
	}
	if protocol.StatusCodeOf(err) != 401 {
		t.Errorf("status = %d, want 401 (error = %v)", protocol.StatusCodeOf(err), err)
	}
	if fake.conversationCalls != 1 {
		t.Errorf("conversation polls = %d, want 1 (401 must not be retried)", fake.conversationCalls)
	}
}

// 没有 access_token 时立刻报错。
func TestEditableExportRequiresAccessToken(t *testing.T) {
	client, _ := newEditableFake(t, "")
	_, err := client.exportEditableFile(context.Background(), protocol.EditableFileRequest{
		Kind:      protocol.EditableKindPPT,
		OutputDir: t.TempDir(),
	}, fastEditableOptions())
	if err == nil || !strings.Contains(err.Error(), "access_token is required") {
		t.Fatalf("error = %v, want access_token required", err)
	}
}

// 未知 kind 必须报错（避免拼出 data/files/xxx 这种野目录）。
func TestEditableExportRejectsUnknownKind(t *testing.T) {
	client, _ := newEditableFake(t, "token-1")
	_, err := client.exportEditableFile(context.Background(), protocol.EditableFileRequest{
		Kind:      "doc",
		OutputDir: t.TempDir(),
	}, fastEditableOptions())
	if err == nil || !strings.Contains(err.Error(), "unknown editable file kind") {
		t.Fatalf("error = %v, want unknown editable file kind", err)
	}
}

// 文件名解析：artifact.name 优先，缺扩展名时按 MIME 补。
func TestResolveEditableOutputName(t *testing.T) {
	ppt := editableSpecFor(protocol.EditableKindPPT)
	cases := []struct {
		name         string
		artifact     editableArtifact
		finalURL     string
		contentDispo string
		contentType  string
		want         string
	}{
		{
			name:     "artifact name wins",
			artifact: editableArtifact{name: "deck.pptx"},
			want:     "deck.pptx",
		},
		{
			name:     "sandbox path fallback",
			artifact: editableArtifact{sandboxPath: "/mnt/data/assets.zip"},
			want:     "assets.zip",
		},
		{
			name:         "content disposition fallback",
			artifact:     editableArtifact{},
			contentDispo: `attachment; filename="deck.pptx"`,
			want:         "deck.pptx",
		},
		{
			name:     "url path fallback",
			artifact: editableArtifact{},
			finalURL: "https://cdn.example/artifact/final.pptx",
			want:     "final.pptx",
		},
		{
			name:        "mime adds extension",
			artifact:    editableArtifact{name: "deck"},
			contentType: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
			want:        "deck.pptx",
		},
		{
			name:     "no name at all uses artifact prefix",
			artifact: editableArtifact{},
			finalURL: "https://cdn.example/",
			want:     "artifact",
		},
	}
	for _, item := range cases {
		got := resolveEditableOutputName(item.artifact, item.finalURL, item.contentDispo, item.contentType, ppt)
		if got != item.want {
			t.Errorf("%s: got %q, want %q", item.name, got, item.want)
		}
	}
}

// 文件名必须只保留 basename（防上游塞 ../ 到产物名里）。
func TestEditableSanitizeFilenameStripsTraversal(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":   "passwd",
		`..\\..\\windows\\x`: `..\\..\\windows\\x`,
		"deck.pptx":          "deck.pptx",
		"":                   "",
	}
	for input, want := range cases {
		if got := editableSanitizeFilename(input); got != want {
			t.Errorf("editableSanitizeFilename(%q) = %q, want %q", input, got, want)
		}
	}
}

// mustDecodeBase64 解出测试用 PNG 的原始字节（断言 file_size 用）。
func mustDecodeBase64(t *testing.T, value string) []byte {
	t.Helper()
	data, err := decodeBase64Image(value)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	return data
}
