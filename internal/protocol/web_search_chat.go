package protocol

import (
	"regexp"
	"strings"
)

// 这一组函数复刻 services/protocol/web_search_tool.py，
// 把上游搜索结果的「引用标记」清成可读文本并抽出 sources 列表。

// annotationMarker 是上游引用标记的分隔符（U+E200 / U+E201 / U+E202）。
//
// 这些是不可见的私用区字符：上游把引用塞进正文，不清掉的话客户端
// 会看到一堆「豆腐块」。Python 同样用这三个字符做正则边界。
const (
	annotationOpen  = "\ue200"
	annotationClose = "\ue201"
	annotationSep   = "\ue202"
)

var (
	// 完整的 \ue200...\ue201 段
	annotationBlockRe = regexp.MustCompile(annotationOpen + `([^` + annotationClose + `]*)` + annotationClose)
	// 未闭合的尾部标记（上游偶发截断）
	annotationOpenTailRe = regexp.MustCompile(annotationOpen + `[^` + annotationClose + `]*$`)
	// 标点前的多余空白
	spaceBeforePunctRe = regexp.MustCompile(`\s+([.,;:!?])`)
	// 纯数字（无效的 cite 内容）
	digitsOnlyRe = regexp.MustCompile(`^\d+$`)
)

// CleanSearchText 把正文里的引用标记替换成可读形式。
//
// 三类标记：
//   - url：`label (url)` 或只有 label/url
//   - cite：取第一个「不是 turn/source/数字」的片段
//   - 其余：同 cite
func CleanSearchText(text string) string {
	replaced := annotationBlockRe.ReplaceAllStringFunc(text, func(match string) string {
		inner := annotationBlockRe.FindStringSubmatch(match)
		if len(inner) < 2 {
			return ""
		}
		parts := strings.Split(inner[1], annotationSep)
		for index := range parts {
			parts[index] = strings.TrimSpace(parts[index])
		}
		kind := ""
		if len(parts) > 0 {
			kind = strings.ToLower(parts[0])
		}
		data := parts
		if len(parts) > 0 {
			data = parts[1:]
		}

		switch kind {
		case "url":
			label := ""
			if len(data) > 0 {
				label = data[0]
			}
			url := ""
			if len(data) > 1 {
				url = data[1]
			}
			if label != "" && isHTTPURL(url) {
				return label + " (" + url + ")"
			}
			if label != "" {
				return label
			}
			return url
		default:
			return readableAnnotationPart(data)
		}
	})

	replaced = annotationOpenTailRe.ReplaceAllString(replaced, "")
	replaced = spaceBeforePunctRe.ReplaceAllString(replaced, "$1")
	return strings.TrimSpace(replaced)
}

// readableAnnotationPart 取第一个有意义的片段。
//
// 跳过 `turn`/`source`/`sources` 前缀与纯数字 —— 它们只是定位信息。
func readableAnnotationPart(parts []string) string {
	for _, part := range parts {
		value := strings.TrimSpace(part)
		lower := strings.ToLower(value)
		if value == "" {
			continue
		}
		if strings.HasPrefix(lower, "turn") ||
			strings.HasPrefix(lower, "source") ||
			digitsOnlyRe.MatchString(value) {
			continue
		}
		return value
	}
	return ""
}

// NormalizedSources 抽出 sources 并去重（按 url）。
func NormalizedSources(result map[string]any) []map[string]string {
	items, _ := result["sources"].([]any)
	output := make([]map[string]string, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		source, ok := item.(map[string]any)
		if !ok {
			continue
		}
		url := strings.TrimSpace(stringOf(source["url"]))
		if url == "" || seen[url] {
			continue
		}
		seen[url] = true
		output = append(output, map[string]string{
			"title":   strings.TrimSpace(stringOf(source["title"])),
			"snippet": strings.TrimSpace(stringOf(source["snippet"])),
			"url":     url,
		})
	}
	return output
}

// TextWithURLCitations 把 sources 追加成正文里的编号列表，并产出 annotations。
//
// 附加格式（逐字对齐 Python）：
//
//	\n\nSources:\n1. <title> - <url>\n2. ...
//
// title 为空时用 url 代替；有 title 时 url 前多一个 " - "。
// annotation 的 start_index/end_index 必须精确覆盖 url 子串 —— 客户端靠它
// 做点击跳转，错一位就跳错。
func TextWithURLCitations(result map[string]any) (string, []map[string]any) {
	text := CleanSearchText(stringOf(result["answer"]))
	annotations := make([]map[string]any, 0, 4)
	sources := NormalizedSources(result)
	if len(sources) == 0 {
		return strings.TrimSpace(text), annotations
	}

	text = strings.TrimRight(text, " \t\n\r")
	if text != "" {
		text += "\n\n"
	}
	text += "Sources:\n"

	for index, source := range sources {
		title := source["title"]
		if title == "" {
			title = source["url"]
		}
		text += itoaValue(index+1) + ". " + title
		if source["url"] != "" {
			if source["title"] != "" {
				text += " - "
			}
			start := len(text)
			text += source["url"]
			annotationTitle := source["title"]
			if annotationTitle == "" {
				annotationTitle = source["url"]
			}
			annotations = append(annotations, map[string]any{
				"type":        "url_citation",
				"start_index": start,
				"end_index":   len(text),
				"url":         source["url"],
				"title":       annotationTitle,
			})
		}
		text += "\n"
	}
	return strings.TrimSpace(text), annotations
}

// SearchQueryFromMessages 取最后一条 user 消息的文本作为搜索词。
func SearchQueryFromMessages(messages []map[string]any) string {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if strings.ToLower(strings.TrimSpace(stringOf(message["role"]))) != "user" {
			continue
		}
		if text := MessageText(message["content"]); text != "" {
			return text
		}
	}
	return ""
}

// isHTTPURL 报告是否是 http(s) 链接。
func isHTTPURL(value string) bool {
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}

// itoaValue 把正整数转成十进制字符串。
func itoaValue(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
