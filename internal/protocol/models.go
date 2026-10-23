package protocol

import (
	"sort"
	"strings"
)

// 图片模型常量，与 utils/helper.py 的 IMAGE_MODELS / PREFIXED_CODEX_IMAGE_MODELS 一致。
const CodexImageModel = "codex-gpt-image-2"

// 图片模型清单。2026-10 实测：上游除 gpt-image-2 外还有 2.5 系列的三个变体，
// 三者产出字节数各不相同（839KB / 853KB / 943KB，同 prompt 1254x1254），
// 确认是不同模型而非别名，因此全部列入。
//
// 注意：这些 slug 不在 /backend-api/models 的返回里（该端点只暴露文本模型），
// 无法通过接口自动发现，只能实测确认后写死。新增模型时同样要实测出图再收录。
var baseImageModels = map[string]bool{
	"gpt-image-2":            true,
	"gpt-image-2.5":          true,
	"gpt-image-2.5-flare":    true,
	"gpt-image-2.5-sunburst": true,
	"codex-gpt-image-2":      true,
}

// prefixedCodexImageModels 是带套餐前缀的 Codex 模型别名。
var prefixedCodexImageModels = map[string]bool{
	"plus-codex-gpt-image-2": true,
	"team-codex-gpt-image-2": true,
	"pro-codex-gpt-image-2":  true,
}

// ImageModels 是全部受支持的图片模型集合。
var ImageModels = func() map[string]bool {
	out := make(map[string]bool, len(baseImageModels)+len(prefixedCodexImageModels))
	for name := range baseImageModels {
		out[name] = true
	}
	for name := range prefixedCodexImageModels {
		out[name] = true
	}
	return out
}()

// SplitImageModel 复刻 split_image_model：把 "team-codex-gpt-image-2" 拆成
// ("team", "codex-gpt-image-2")；不带前缀时返回 ("", 原值)。
func SplitImageModel(model any) (planType string, baseModel string) {
	name := strings.ToLower(strings.TrimSpace(stringOf(model)))
	if name == "" {
		return "", ""
	}
	for prefix := range prefixedCodexImageModels {
		// 前缀形如 "team-codex-gpt-image-2"，用 "-" 切出套餐名。
		if name == prefix {
			plan := strings.TrimSuffix(prefix, "-"+CodexImageModel)
			return plan, CodexImageModel
		}
	}
	if baseImageModels[name] {
		return "", name
	}
	return "", ""
}

// IsSupportedImageModel 报告模型是否是受支持的图片模型。
func IsSupportedImageModel(model any) bool {
	_, base := SplitImageModel(model)
	return base != ""
}

// IsCodexImageModel 报告模型是否走 Codex 链路。
func IsCodexImageModel(model any) bool {
	_, base := SplitImageModel(model)
	return base == CodexImageModel
}

// ListModelsResult 复刻 openai_v1_models.list_models 的返回：
// 上游模型目录 + 按账号类型动态追加的图片模型。
//
// upstreamModels 是 model_catalog_service 聚合后的 data 数组；
// accountTypes 是号池里各账号的 (type, source_type) 组合。
func ListModelsResult(upstreamModels []map[string]any, accounts []AccountSummary) map[string]any {
	data := make([]map[string]any, 0, len(upstreamModels)+8)
	seen := make(map[string]bool, len(upstreamModels)+8)
	for _, item := range upstreamModels {
		id := strings.TrimSpace(stringOf(item["id"]))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		data = append(data, item)
	}

	dynamic := map[string]bool{}
	hasWebImageAccount := false
	codexTypes := map[string]bool{}
	for _, account := range accounts {
		if account.SourceType != "codex" {
			hasWebImageAccount = true
		}
		if account.SourceType == "codex" && account.Type != "" {
			codexTypes[account.Type] = true
		}
	}
	// 任意 web 账号都能出图（Python 版是 web_image_accounts 非空即加 gpt-image-2）。
	if len(accounts) > 0 || hasWebImageAccount {
		// web 账号可用全部 web 图片模型（含 2.5 系列）。
		for name := range baseImageModels {
			if name != CodexImageModel {
				dynamic[name] = true
			}
		}
	}
	if codexTypes["Plus"] || codexTypes["Team"] || codexTypes["Pro"] {
		dynamic[CodexImageModel] = true
	}
	if codexTypes["Plus"] {
		dynamic["plus-"+CodexImageModel] = true
	}
	if codexTypes["Team"] {
		dynamic["team-"+CodexImageModel] = true
	}
	if codexTypes["Pro"] {
		dynamic["pro-"+CodexImageModel] = true
	}

	dynamicNames := make([]string, 0, len(dynamic))
	for name := range dynamic {
		dynamicNames = append(dynamicNames, name)
	}
	sort.Strings(dynamicNames)
	for _, name := range dynamicNames {
		if seen[name] {
			continue
		}
		seen[name] = true
		data = append(data, map[string]any{
			"id":         name,
			"object":     "model",
			"created":    0,
			"owned_by":   "chatgpt2api",
			"permission": []any{},
			"root":       name,
			"parent":     nil,
		})
	}

	return map[string]any{"object": "list", "data": data}
}

// AccountSummary 是 ListModelsResult 需要的账号摘要（只含两个字段，
// 避免 protocol 依赖 service 的完整 Account 类型）。
type AccountSummary struct {
	Type       string
	SourceType string
}
