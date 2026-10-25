package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/IFuZYI/chatgpt2api/internal/service"
)

// editableTaskTimeout 是单次导出的上限。
//
// 比上游链路自己的 1200s（EDITABLE_FILE_TIMEOUT_SECS）多留 5 分钟：
// 上传参考图、下载产物都发生在轮询之外，卡在同一个 1200s 上会让
// 「轮询刚好在最后一刻成功、下载却已被 ctx 掐断」变成一个必然发生的竞态。
const editableTaskTimeout = 25 * time.Minute

// RegisterEditableFileTaskRunner 把可编辑文件任务的执行体注入任务服务。
//
// 与 RegisterImageTaskRunners 同理：任务在后台 goroutine 里跑，走不到 HTTP
// handler，所以执行体必须由 api 层显式注入；漏掉这一步会让所有 PPT/PSD
// 任务在开跑瞬间失败并报 "runner is not configured"。
func (d *Deps) RegisterEditableFileTaskRunner() {
	if d.EditableTasks == nil {
		return
	}
	d.EditableTasks.SetRunner(d.runEditableFileTask)
	d.EditableTasks.SetAccounts(d.Accts)
}

// runEditableFileTask 执行一次 PPT/PSD 导出。
//
// 复刻 editable_file_task_service._run_task 的 try 块：
// 取号（由服务在调用前完成）→ 建上游 → 导出 → 拼 result → 记调用日志。
func (d *Deps) runEditableFileTask(taskCtx service.EditableFileTaskContext) (service.EditableFileTaskResult, error) {
	call := d.beginCall(editableTaskSummary(taskCtx.Kind), "/v1/"+taskCtx.Kind+"/generations",
		protocol.EditableFileModel, taskCtx.KeyName)

	if d.EditableUpstream == nil {
		err := errors.New("editable file upstream factory is not configured")
		call.finish(err, "", nil)
		return service.EditableFileTaskResult{}, err
	}
	if strings.TrimSpace(taskCtx.AccessToken) == "" {
		err := errors.New("access_token is required for editable file export")
		call.finish(err, "", nil)
		return service.EditableFileTaskResult{}, err
	}

	// 任务脱离请求 ctx（客户端早已拿到 202 语义的任务对象），但也不能裸跑：
	// 导出可能跑 20 分钟，必须能被服务关闭取消。
	ctx, cancel := context.WithTimeout(context.Background(), editableTaskTimeout)
	defer cancel()

	upstream, closeFn, err := d.EditableUpstream(ctx, taskCtx.AccessToken)
	if err != nil {
		call.finish(err, "", nil)
		return service.EditableFileTaskResult{}, err
	}
	defer closeFn()

	exported, exportErr := upstream.ExportEditableFile(ctx, protocol.EditableFileRequest{
		Kind:         taskCtx.Kind,
		Prompt:       taskCtx.Prompt,
		Base64Images: taskCtx.Base64Images,
		OutputDir:    taskCtx.OutputDir,
	})
	if exportErr != nil {
		call.finish(exportErr, "", nil)
		return service.EditableFileTaskResult{}, exportErr
	}

	result := service.EditableFileTaskResult{
		ConversationID: exported.ConversationID,
		PrimaryPath:    exported.PrimaryPath,
		ZipPath:        exported.ZipPath,
	}

	// 产物落在 data/files 之外：说明上游把路径带歪了（或目录被改）。
	// 这种情况必须报错，不能返回一个指向不存在文件的链接。
	primaryURL := d.EditableTasks.FileURL(exported.PrimaryPath, taskCtx.BaseURL)
	zipURL := d.EditableTasks.FileURL(exported.ZipPath, taskCtx.BaseURL)
	if primaryURL == "" || zipURL == "" {
		err := fmt.Errorf("exported files are outside the editable file root: %s / %s",
			exported.PrimaryPath, exported.ZipPath)
		call.finish(err, "", nil)
		return service.EditableFileTaskResult{}, err
	}

	call.finish(nil, d.Accts.EditableFileAccountEmail(taskCtx.AccessToken), []string{primaryURL, zipURL})
	return result, nil
}
