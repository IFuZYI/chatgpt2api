package protocol

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// 本文件复刻 services/protocol/openai_search.py 与
// services/openai_backend_api.py 的搜索相关常量/抽取逻辑（行号见各函数注释）。
//
// 搜索与图片同源：都是 /backend-api/f/conversation/prepare → /f/conversation
// 建会话，再轮询 /backend-api/conversation/{id} 取结果。区别只在请求体
// （system_hints=["search"]、force_use_search=true）与结果抽取方式。

// SearchModel 是 /v1/search 固定使用的上游模型（Python SEARCH_MODEL）。
const SearchModel = "gpt-5-5"

// SearchDoneStatus 复刻 SEARCH_DONE_STATUS：
// answer 非空且 status 命中这里时立即返回，不必等 answer 稳定。
var SearchDoneStatus = map[string]bool{
	"finished_successfully":       true,
	"finished_partial_completion": true,
}

var (
	// searchConversationIDRe 复刻 SEARCH_CONVERSATION_ID_RE。
	// SSE 的 payload 是字符串，先按正则捞 conversation_id，再退回 JSON 解析。
	searchConversationIDRe = regexp.MustCompile(`"conversation_id"\s*:\s*"([^"]+)"`)

	// searchURLRe 复刻 SEARCH_URL_RE：正文里的裸 URL。
	// 排除空白、引号、尖括号与中英文括号/方括号/花括号 —— 这些是 markdown 或
	// 引用标记的分隔符，吃进去会得到带尾巴的 URL。
	// 比 Python 多排了 \p{Z}：Python 的 \s 在 str 模式下含 Unicode 分隔符，
	// Go 的 \s 只有 ASCII 空白，不补会让 URL 吞掉不间断空格。
	searchURLRe = regexp.MustCompile(`https?://[^\s\p{Z}"'<>）)\]}]+`)
)

// SearchUpstream 是搜索链路需要的最小上游能力。
type SearchUpstream interface {
	// Search 跑一次联网搜索：prepare → bootstrap → conversation → 轮询结果。
	Search(ctx context.Context, prompt, model string) (map[string]any, error)
}

// SearchSource 是一条搜索来源（复刻 _extract_search_sources 的字段）。
type SearchSource struct {
	Title      string
	URL        string
	Snippet    string
	SourceType string
}

// SearchResult 复刻 _extract_search_result 的返回结构。
type SearchResult struct {
	ConversationID     string
	Status             string
	Answer             string
	Sources            []SearchSource
	AssistantMessageID string
	CreateTime         float64
}

// Map 输出 Python 字典的键形状（/v1/search 的响应体）。
//
// 之所以转成 map 而不是直接序列化结构体：Python 侧就是一个 dict，
// 调用方（含 _account_email 的注入）都按 dict 处理。
func (r SearchResult) Map() map[string]any {
	sources := make([]any, 0, len(r.Sources))
	for _, source := range r.Sources {
		sources = append(sources, map[string]any{
			"title":       source.Title,
			"url":         source.URL,
			"snippet":     source.Snippet,
			"source_type": source.SourceType,
		})
	}
	return map[string]any{
		"conversation_id":      r.ConversationID,
		"status":               r.Status,
		"answer":               r.Answer,
		"sources":              sources,
		"assistant_message_id": r.AssistantMessageID,
		"create_time":          r.CreateTime,
	}
}

// ExtractSearchResult 复刻 _extract_search_result（openai_backend_api.py:1947-1969）：
// 从 conversation["mapping"] 里取 create_time 最大的 assistant 消息。
func ExtractSearchResult(conversationID string, conversation map[string]any) SearchResult {
	message := latestAssistantMessage(conversation)
	metadata, _ := message["metadata"].(map[string]any)
	finishDetails, _ := metadata["finish_details"].(map[string]any)

	status := strings.TrimSpace(firstSearchString(
		finishDetails["type"],
		metadata["status"],
		FindSearchValue(message, "status"),
	))

	answer := SearchMessageText(message)
	sources := ExtractSearchSources(message)
	for _, url := range searchURLRe.FindAllString(answer, -1) {
		cleaned := CleanSearchURL(url)
		if cleaned == "" || searchSourceExists(sources, cleaned) {
			continue
		}
		// 正文里的 URL 只补 url，其余字段留空（与 Python 一致）。
		sources = append(sources, SearchSource{URL: cleaned})
	}

	return SearchResult{
		ConversationID:     conversationID,
		Status:             status,
		Answer:             answer,
		Sources:            sources,
		AssistantMessageID: searchValueString(message["id"]),
		CreateTime:         floatValueOf(message["create_time"]),
	}
}

// latestAssistantMessage 取 mapping 里 create_time 最大的 assistant 消息。
func latestAssistantMessage(conversation map[string]any) map[string]any {
	mapping, _ := conversation["mapping"].(map[string]any)
	var latest map[string]any
	latestCreateTime := 0.0
	for _, node := range mapping {
		nodeMap, ok := node.(map[string]any)
		if !ok {
			continue
		}
		message, ok := nodeMap["message"].(map[string]any)
		if !ok {
			continue
		}
		author, _ := message["author"].(map[string]any)
		if searchValueString(author["role"]) != "assistant" {
			continue
		}
		createTime := floatValueOf(message["create_time"])
		if latest == nil || createTime > latestCreateTime {
			latest = message
			latestCreateTime = createTime
		}
	}
	return latest
}

// ExtractSearchSources 复刻 _extract_search_sources（:1971-1983）：
// 递归遍历所有 dict，取 url/link/source_url/metadata.url，按 url 去重。
func ExtractSearchSources(payload any) []SearchSource {
	sources := make([]SearchSource, 0, 4)
	for _, object := range walkSearchDicts(payload) {
		metadata, _ := object["metadata"].(map[string]any)
		url := CleanSearchURL(firstSearchString(
			object["url"],
			object["link"],
			object["source_url"],
			metadata["url"],
		))
		if url == "" || searchSourceExists(sources, url) {
			continue
		}
		sources = append(sources, SearchSource{
			Title: strings.TrimSpace(firstSearchString(
				object["title"],
				object["name"],
				object["source"],
			)),
			URL: url,
			Snippet: strings.TrimSpace(firstSearchString(
				object["snippet"],
				object["text"],
				object["description"],
			)),
			SourceType: strings.TrimSpace(firstSearchString(
				object["type"],
				object["source_type"],
			)),
		})
	}
	return sources
}

// SearchMessageText 复刻 _search_message_text（:1985-1998）。
//
// content 是 dict 时取 text 与 parts（str 直接用，dict 取 text/summary/content），
// 每段 strip 后按 "\n" 拼接；content 是字符串时直接用。
func SearchMessageText(message any) string {
	messageMap, _ := message.(map[string]any)
	parts := make([]string, 0, 4)
	switch content := messageMap["content"].(type) {
	case map[string]any:
		if text, ok := content["text"].(string); ok {
			parts = append(parts, text)
		}
		rawParts, _ := content["parts"].([]any)
		for _, part := range rawParts {
			switch entry := part.(type) {
			case string:
				parts = append(parts, entry)
			case map[string]any:
				for _, key := range []string{"text", "summary", "content"} {
					if value := searchValueString(entry[key]); value != "" {
						parts = append(parts, value)
					}
				}
			}
		}
	case string:
		parts = append(parts, content)
	}
	return joinSearchParts(parts)
}

// FindSearchValue 复刻 _find_search_value（:2000-2016）：
// 字符串先按 conversation_id 正则匹配，再尝试 JSON 解析；dict 先直取键，
// 再递归所有值；list 逐个递归。返回第一个非空命中。
//
// 导出是因为上游要拿它解析 SSE 的每一行 payload。
func FindSearchValue(payload any, key string) string {
	switch typed := payload.(type) {
	case string:
		if key == "conversation_id" {
			if match := searchConversationIDRe.FindStringSubmatch(typed); len(match) > 1 {
				return match[1]
			}
		}
		var decoded any
		if err := json.Unmarshal([]byte(typed), &decoded); err != nil {
			return ""
		}
		return FindSearchValue(decoded, key)
	case map[string]any:
		if value := searchValueString(typed[key]); value != "" {
			return value
		}
		for _, name := range sortedMapKeys(typed) {
			if found := FindSearchValue(typed[name], key); found != "" {
				return found
			}
		}
		return ""
	case []any:
		for _, item := range typed {
			if found := FindSearchValue(item, key); found != "" {
				return found
			}
		}
		return ""
	default:
		return ""
	}
}

// CleanSearchURL 复刻 _clean_search_url（:2025-2026）：
// strip 后去掉结尾的标点（URL 常被句号/逗号/中文标点粘住）。
func CleanSearchURL(value any) string {
	return strings.TrimRight(strings.TrimSpace(searchValueString(value)), ".,;，。；")
}

// walkSearchDicts 复刻 _walk_search_dicts（:2018-2023）：dict 自己 + 所有嵌套 dict。
//
// 键按字母序遍历而不是 map 随机序：Python 的 dict 保留 JSON 文档顺序，
// Go 的 map 不保留，排序至少保证结果可复现（顺序差异记在报告里）。
func walkSearchDicts(payload any) []map[string]any {
	switch typed := payload.(type) {
	case map[string]any:
		out := make([]map[string]any, 0, len(typed)+1)
		out = append(out, typed)
		for _, name := range sortedMapKeys(typed) {
			out = append(out, walkSearchDicts(typed[name])...)
		}
		return out
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, walkSearchDicts(item)...)
		}
		return out
	default:
		return nil
	}
}

// sortedMapKeys 按字母序返回 map 的键，保证递归遍历可复现。
func sortedMapKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}

// searchValueString 取字符串字段：非字符串一律当空。
// 上游这些字段（url/title/type）实测都是字符串，不做 Python 的 str() 兜底。
func searchValueString(value any) string {
	text, _ := value.(string)
	return text
}

// firstSearchString 等价 Python 的 `a or b or c or ""`（只处理字符串）。
func firstSearchString(values ...any) string {
	for _, value := range values {
		if text := searchValueString(value); text != "" {
			return text
		}
	}
	return ""
}

// joinSearchParts 复刻 `"\n".join(part.strip() for part in parts if part.strip()).strip()`。
func joinSearchParts(parts []string) string {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}

// searchSourceExists 判断 url 是否已在来源列表里（Python 用 all(item["url"] != url ...)）。
func searchSourceExists(sources []SearchSource, url string) bool {
	for _, source := range sources {
		if source.URL == url {
			return true
		}
	}
	return false
}
