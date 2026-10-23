package protocol

import "strings"

// PromptWithGlobalSystem 复刻 Python 的 prompt_with_global_system：
// 把全局系统提示词前置拼进 prompt。
//
// 为什么需要它：config.global_system_prompt 是管理员设置页里的可见配置项，
// 保存成功、界面显示，但请求链路从不读取 —— 用户以为统一画风/统一约束
// 已生效，实际零效果。Python 在 conversation.py:697 对图片与文本都拼。
//
// 前缀顺序与 Python 一致："{system}\n\n{prompt}"，system 为空时原样返回。
func PromptWithGlobalSystem(prompt, systemPrompt string) string {
	trimmedSystem := strings.TrimSpace(systemPrompt)
	if trimmedSystem == "" {
		return prompt
	}
	return trimmedSystem + "\n\n" + prompt
}
