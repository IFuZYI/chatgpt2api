package protocol

import "context"

// 可编辑文件（PPT/PSD）链路的共享契约。
//
// 与图片/搜索链路同源：都是 /backend-api/f/conversation/prepare → /f/conversation，
// 区别是模型换成 gpt-5-5-thinking、参考图必须先上传、轮询到的产物是文件
// （pptx/psd + zip）而不是图片。Python 的权威实现见
// services/openai_backend_api.py:1163-1780 与 services/editable_file_task_service.py。

// 可编辑文件的任务类型。这两个字面量同时是任务记录的 kind 字段与
// data/files/{kind}/ 下的目录名，不能改。
const (
	EditableKindPPT = "ppt"
	EditableKindPSD = "psd"
)

// EditableFileModel 复刻 EDITABLE_FILE_MODEL：可编辑文件链路固定用这个上游模型。
const EditableFileModel = "gpt-5-5-thinking"

// EditableFileRequest 是一次可编辑文件导出的输入。
//
// Prompt 是**用户补充需求**：两个固定模板由上游拼在前面（_editable_prompt），
// 调用方不要自己拼模板。
type EditableFileRequest struct {
	// Kind 是 EditableKindPPT / EditableKindPSD，决定模板、产物后缀与 MIME。
	Kind string
	// Prompt 是用户补充需求，可为空。
	Prompt string
	// Base64Images 是参考图（可带 data: URI 前缀）。PSD 链路至少一张。
	Base64Images []string
	// OutputDir 是产物落盘目录（已存在的绝对路径）。
	OutputDir string
}

// EditableFileExportResult 是导出结果：主产物（pptx/psd）与素材 zip。
type EditableFileExportResult struct {
	ConversationID string
	PrimaryPath    string
	ZipPath        string
}

// EditableFileUpstream 是可编辑文件链路需要的最小上游能力。
type EditableFileUpstream interface {
	// ExportEditableFile 跑完整链路：上传参考图 → prepare → conversation
	// → 轮询产物 → 下载到 OutputDir。
	ExportEditableFile(ctx context.Context, request EditableFileRequest) (EditableFileExportResult, error)
}
