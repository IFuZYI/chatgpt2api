package service

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/config"
)

// 图片扩展名白名单。
var imageExtensions = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".webp": true,
}

// ImageItem 是 /api/images 返回的单个条目。
//
// 字段集与 image_storage_service.list_items 的输出一致：前端按
// path / url / thumbnail_url / tags / date / created_at 渲染。
type ImageItem map[string]any

// ImageService 复刻 services/image_service.py + image_storage_service.py。
//
// 存储分流由 image_storage.mode 决定（local / webdav / both），
// 见 SaveImageBytes；读取时本地优先、索引标记 webdav 才回退远端，见 ReadImage。
type ImageService struct {
	cfg *config.Store

	indexMu sync.Mutex
}

func NewImageService(cfg *config.Store) *ImageService {
	return &ImageService{cfg: cfg}
}

// imageStorageMode 复刻 Python 的 mode() + save() 里的白名单校验：
// 非法值（含空、大小写不符、未知模式）一律按 local 处理。
func imageStorageMode(settings map[string]any) string {
	switch mode := strings.TrimSpace(stringValue(settings["mode"])); mode {
	case "local", "webdav", "both":
		return mode
	default:
		return "local"
	}
}

func (s *ImageService) indexPath() string {
	return filepath.Join(s.cfg.DataDir(), "image_index.json")
}

func (s *ImageService) tagsPath() string {
	return filepath.Join(s.cfg.DataDir(), "image_tags.json")
}

// safeRelativePath 复刻 Python 的 _safe_relative_path：
// 反斜杠归一、去前导斜杠、拒绝空段与上跳。
func safeRelativePath(raw string) (string, error) {
	value := strings.TrimLeft(strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/"), "/")
	if value == "" {
		return "", errImageNotFound
	}
	parts := strings.Split(value, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errImageNotFound
		}
		clean = append(clean, part)
	}
	return strings.Join(clean, "/"), nil
}

var errImageNotFound = fmt.Errorf("image not found")

// localImagePath 把相对路径解析到 images 根目录下，拒绝越界。
func (s *ImageService) localImagePath(rel string) (string, error) {
	safe, err := safeRelativePath(rel)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(s.cfg.ImagesDir())
	if err != nil {
		return "", err
	}
	full, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(safe)))
	if err != nil {
		return "", err
	}
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", errImageNotFound
	}

	// 必须解析符号链接后再校验一次。
	//
	// filepath.Abs 只做字符串层面的 Clean，不跟随 symlink。
	// 攻击者在 data/images/ 下放一个指向 ../../config.json 的软链，
	// 上面的前缀检查会通过（路径字面在 root 下），
	// 于是未鉴权的 GET /images/<name> 就能读出 config.json
	// （含 auth-key）。已实测复现。
	resolvedRoot, rootErr := filepath.EvalSymlinks(root)
	resolvedFull, fullErr := filepath.EvalSymlinks(full)
	if rootErr == nil && fullErr == nil {
		if resolvedFull != resolvedRoot &&
			!strings.HasPrefix(resolvedFull, resolvedRoot+string(os.PathSeparator)) {
			return "", errImageNotFound
		}
	}
	return full, nil
}

func isImageRel(rel string) bool {
	safe, err := safeRelativePath(rel)
	if err != nil {
		return false
	}
	return imageExtensions[strings.ToLower(filepath.Ext(safe))]
}

// ---- 索引 ----

func (s *ImageService) loadIndexLocked() map[string]map[string]any {
	payload, err := os.ReadFile(s.indexPath())
	if err != nil {
		return map[string]map[string]any{}
	}
	var wrapper struct {
		Items map[string]map[string]any `json:"items"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		return map[string]map[string]any{}
	}
	out := make(map[string]map[string]any, len(wrapper.Items))
	for key, value := range wrapper.Items {
		if isImageRel(key) && value != nil {
			out[key] = value
		}
	}
	return out
}

func (s *ImageService) saveIndexLocked(items map[string]map[string]any) {
	payload, err := json.MarshalIndent(map[string]any{"items": items}, "", "  ")
	if err != nil {
		return
	}
	payload = append(payload, '\n')
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		_ = os.WriteFile(s.indexPath(), payload, 0o644)
		return
	}
	if err := os.Rename(tmp, s.indexPath()); err != nil {
		_ = os.WriteFile(s.indexPath(), payload, 0o644)
		_ = os.Remove(tmp)
	}
}

// publicImageURL 复刻 _public_url：public_base_url 优先，否则 base_url + /images/。
func (s *ImageService) publicImageURL(rel, baseURL string) string {
	settings := s.cfg.ImageStorageSettings()
	if publicBase, _ := settings["public_base_url"].(string); strings.TrimSpace(publicBase) != "" {
		return strings.TrimRight(strings.TrimSpace(publicBase), "/") + "/" + rel
	}
	root := strings.TrimRight(baseURL, "/")
	if root == "" {
		root = strings.TrimRight(s.cfg.BaseURL(), "/")
	}
	return root + "/images/" + rel
}

// ThumbnailURL 复刻 Python 的 thumbnail_url。
func ThumbnailURL(baseURL, rel string) string {
	return strings.TrimRight(baseURL, "/") + "/image-thumbnails/" + rel
}

// ListItems 复刻 image_storage_service.list_items：
// 扫描磁盘补齐索引、清理失效条目、按 created_at 倒序。
func (s *ImageService) ListItems(baseURL, startDate, endDate string) []ImageItem {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()

	indexed := s.loadIndexLocked()
	root := s.cfg.ImagesDir()
	changed := false

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !isImageRel(info.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if _, exists := indexed[rel]; exists {
			return nil
		}
		item := map[string]any{
			"rel":        rel,
			"path":       rel,
			"name":       info.Name(),
			"size":       info.Size(),
			"created_at": info.ModTime().Format("2006-01-02 15:04:05"),
			"storage":    "local",
			"local":      true,
			"webdav":     false,
		}
		item["date"] = dateFromRel(rel, info.ModTime())
		if width, height, ok := imageDimensions(path); ok {
			item["width"], item["height"] = width, height
		}
		indexed[rel] = item
		changed = true
		return nil
	})

	items := make([]ImageItem, 0, len(indexed))
	for rel, item := range indexed {
		if !isImageRel(rel) {
			delete(indexed, rel)
			changed = true
			continue
		}
		full, err := s.localImagePath(rel)
		local := err == nil && fileExists(full)
		webdav := truthy(item["webdav"])
		if !local && !webdav {
			delete(indexed, rel)
			changed = true
			continue
		}
		storage := "local"
		switch {
		case local && webdav:
			storage = "both"
		case webdav:
			storage = "webdav"
		}
		if item["local"] != local || stringValue(item["storage"]) != storage {
			item = copyMap(item)
			item["local"] = local
			item["storage"] = storage
			indexed[rel] = item
			changed = true
		}
		day := stringValue(item["date"])
		if startDate != "" && day < startDate {
			continue
		}
		if endDate != "" && day > endDate {
			continue
		}
		entry := copyMap(item)
		entry["rel"] = rel
		entry["path"] = rel
		entry["url"] = s.publicImageURL(rel, baseURL)
		items = append(items, entry)
	}
	if changed {
		s.saveIndexLocked(indexed)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return stringValue(items[i]["created_at"]) > stringValue(items[j]["created_at"])
	})
	return items
}

// dateFromRel 复刻 Python：路径前三段是 YYYY/MM/DD 时用它，否则用 mtime。
func dateFromRel(rel string, modTime time.Time) string {
	parts := strings.Split(rel, "/")
	if len(parts) >= 4 {
		return strings.Join(parts[:3], "-")
	}
	return modTime.Format("2006-01-02")
}

// ListResponse 复刻 /api/images 的返回：items + 按日期分组。
func (s *ImageService) ListResponse(baseURL, startDate, endDate string) map[string]any {
	s.cfg.CleanupOldImages()
	s.CleanupThumbnails()

	allTags := s.loadTags()
	rawItems := s.ListItems(baseURL, startDate, endDate)
	items := make([]map[string]any, 0, len(rawItems))
	groupOrder := make([]string, 0)
	groups := map[string][]map[string]any{}

	for _, item := range rawItems {
		rel := stringValue(item["path"])
		entry := copyMap(item)
		entry["url"] = s.publicImageURL(rel, baseURL)
		entry["thumbnail_url"] = ThumbnailURL(baseURL, rel)
		entry["tags"] = tagsOrDefault(allTags, rel)
		items = append(items, entry)

		date := stringValue(entry["date"])
		if _, seen := groups[date]; !seen {
			groupOrder = append(groupOrder, date)
		}
		groups[date] = append(groups[date], entry)
	}

	groupList := make([]map[string]any, 0, len(groupOrder))
	for _, date := range groupOrder {
		groupList = append(groupList, map[string]any{"date": date, "items": groups[date]})
	}
	return map[string]any{"items": items, "groups": groupList}
}

// StorageStats 复刻 /api/images/storage 的返回。
func (s *ImageService) StorageStats() map[string]any {
	root := s.cfg.ImagesDir()
	var total, used, free uint64
	if stat, err := diskUsage(root); err == nil {
		total, used, free = stat.total, stat.used, stat.free
	}

	var count int
	var size int64
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		count++
		size += info.Size()
		return nil
	})

	const mb = 1024 * 1024
	return map[string]any{
		"disk_total_mb":    int(total / mb),
		"disk_used_mb":     int(used / mb),
		"disk_free_mb":     int(free / mb),
		"image_count":      count,
		"image_size_mb":    int(size / mb),
		"image_size_bytes": size,
	}
}

// ThumbnailPath 返回某图片的缩略图路径（不保证存在）。
func (s *ImageService) ThumbnailPath(rel string) (string, error) {
	safe, err := safeRelativePath(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.cfg.ImageThumbnailsDir(), filepath.FromSlash(safe)+".png"), nil
}

// EnsureThumbnail 生成 320x320 缩略图（LANCZOS）。已存在则直接返回。
//
// 源图本地没有时按 get_bytes 的规则回退远端（Python 的
// `io.BytesIO(image_storage_service.get_bytes(...))` 分支）——
// 否则 webdav 模式下每张图的缩略图都 404，管理页只剩文字。
func (s *ImageService) EnsureThumbnail(rel string) (string, error) {
	target, err := s.ThumbnailPath(rel)
	if err != nil {
		return "", err
	}
	if fileExists(target) {
		return target, nil
	}
	source, err := s.localImagePath(rel)
	if err != nil {
		return "", err
	}
	if fileExists(source) {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if err := writeThumbnail(source, target, 320, 320); err != nil {
			return "", err
		}
		return target, nil
	}

	payload, _, readErr := s.ReadImage(rel)
	if readErr != nil {
		return "", errImageNotFound
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if err := writeThumbnailFromBytes(payload, target, 320, 320); err != nil {
		return "", err
	}
	return target, nil
}

// CleanupThumbnails 删除源图已消失的缩略图，并清空目录。
func (s *ImageService) CleanupThumbnails() int {
	root := s.cfg.ImageThumbnailsDir()
	removed := 0
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		source := strings.TrimSuffix(filepath.ToSlash(rel), ".png")
		if full, err := s.localImagePath(source); err == nil && fileExists(full) {
			return nil
		}
		if os.Remove(path) == nil {
			removed++
		}
		return nil
	})
	removeEmptyDirs(root)
	return removed
}

// ReadImage 返回图片字节与 MIME，用于 /images/{path}。
//
// 复刻 Python get_bytes：本地文件优先；本地缺失时，只有索引标记了
// webdav:true 才回退远端读取（否则 404，不能把「没存过」当「远端有」）。
func (s *ImageService) ReadImage(rel string) ([]byte, string, error) {
	full, err := s.localImagePath(rel)
	if err != nil {
		return nil, "", err
	}
	if payload, readErr := os.ReadFile(full); readErr == nil {
		return payload, mimeForPath(full), nil
	}

	safe, err := safeRelativePath(rel)
	if err != nil {
		return nil, "", err
	}
	if !isImageRel(safe) || !s.indexedOnWebDAV(safe) {
		return nil, "", errImageNotFound
	}
	payload, remoteErr := s.fetchRemoteImage(safe)
	if remoteErr != nil {
		return nil, "", remoteErr
	}
	return payload, mimeForPath(full), nil
}

// indexedOnWebDAV 报告索引里该条目是否标记为已存远端。
func (s *ImageService) indexedOnWebDAV(rel string) bool {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	item, exists := s.loadIndexLocked()[rel]
	return exists && truthy(item["webdav"])
}

// fetchRemoteImage 从 WebDAV 取一张图。
//
// 没有请求 ctx：调用方（HTTP 路由 / zip 下载）都没把 ctx 传进来，
// 与 Python 一样只靠客户端超时兜底。
func (s *ImageService) fetchRemoteImage(rel string) ([]byte, error) {
	client := NewWebDAVClient(s.cfg.ImageStorageSettings())
	if client.BaseURL == "" {
		return nil, fmt.Errorf("WebDAV URL 不能为空")
	}
	return client.Get(context.Background(), rel)
}

// deleteRemoteImage 删除远端副本（索引未标 webdav 时直接跳过）。
//
// 返回 true 表示远端确实删掉了。远端失败不阻断本地删除与索引清理：
// 与 Python 一致 —— 本地已经删了，再因为远端 500 把索引留着，
// 列表里就会出现一条打不开的僵尸条目。
func (s *ImageService) deleteRemoteImage(rel string) bool {
	safe, err := safeRelativePath(rel)
	if err != nil || !isImageRel(safe) || !s.indexedOnWebDAV(safe) {
		return false
	}
	client := NewWebDAVClient(s.cfg.ImageStorageSettings())
	if client.BaseURL == "" {
		return false
	}
	removed, err := client.Delete(context.Background(), safe)
	return err == nil && removed
}

// DeleteImages 复刻 delete_images：支持按路径或按日期范围全删。
//
// 索引标记 webdav:true 的条目还要删远端 —— 只删本地会让 webdav 模式的
// 用户在远端永久留一份「删不掉的图」（本地已无文件，列表也查不到）。
func (s *ImageService) DeleteImages(paths []string, startDate, endDate string, allMatching bool) map[string]any {
	targets := paths
	if allMatching {
		targets = make([]string, 0)
		for _, item := range s.ListItems("", startDate, endDate) {
			targets = append(targets, stringValue(item["path"]))
		}
	}

	removed := 0
	for _, rel := range targets {
		full, err := s.localImagePath(rel)
		if err != nil {
			continue
		}
		deleted := false
		if fileExists(full) {
			if os.Remove(full) == nil {
				deleted = true
			}
		}
		if s.deleteRemoteImage(rel) {
			deleted = true
		}
		if deleted {
			removed++
		}
		if thumb, err := s.ThumbnailPath(rel); err == nil && fileExists(thumb) {
			_ = os.Remove(thumb)
		}
		s.removeTags(rel)
	}

	s.indexMu.Lock()
	indexed := s.loadIndexLocked()
	changed := false
	for _, rel := range targets {
		if _, exists := indexed[rel]; exists {
			delete(indexed, rel)
			changed = true
		}
	}
	if changed {
		s.saveIndexLocked(indexed)
	}
	s.indexMu.Unlock()

	removeEmptyDirs(s.cfg.ImagesDir())
	removeEmptyDirs(s.cfg.ImageThumbnailsDir())
	return map[string]any{"removed": removed}
}

// ---- 标签 ----

func (s *ImageService) loadTags() map[string][]string {
	payload, err := os.ReadFile(s.tagsPath())
	if err != nil {
		return map[string][]string{}
	}
	var raw map[string][]string
	if err := json.Unmarshal(payload, &raw); err != nil {
		return map[string][]string{}
	}
	if raw == nil {
		return map[string][]string{}
	}
	return raw
}

func (s *ImageService) saveTags(data map[string][]string) {
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	payload = append(payload, '\n')
	_ = os.MkdirAll(filepath.Dir(s.tagsPath()), 0o755)
	_ = os.WriteFile(s.tagsPath(), payload, 0o644)
}

// AllTags 复刻 get_all_tags：去重并保持首次出现顺序。
func (s *ImageService) AllTags() []string {
	data := s.loadTags()
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, tags := range data {
		for _, tag := range tags {
			if tag == "" || seen[tag] {
				continue
			}
			seen[tag] = true
			out = append(out, tag)
		}
	}
	return out
}

// SetTags 复刻 set_tags：清洗、去重、空则删除条目。
func (s *ImageService) SetTags(rel string, tags []string) []string {
	data := s.loadTags()
	cleaned := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		cleaned = append(cleaned, trimmed)
	}
	if len(cleaned) > 0 {
		data[rel] = cleaned
	} else {
		delete(data, rel)
	}
	s.saveTags(data)
	return cleaned
}

func (s *ImageService) removeTags(rel string) {
	data := s.loadTags()
	if _, exists := data[rel]; !exists {
		return
	}
	delete(data, rel)
	s.saveTags(data)
}

// DeleteTag 从所有图片里移除某标签，返回受影响图片数。
func (s *ImageService) DeleteTag(tag string) int {
	data := s.loadTags()
	count := 0
	for rel, tags := range data {
		kept := make([]string, 0, len(tags))
		hit := false
		for _, item := range tags {
			if item == tag {
				hit = true
				continue
			}
			kept = append(kept, item)
		}
		if !hit {
			continue
		}
		count++
		if len(kept) > 0 {
			data[rel] = kept
		} else {
			delete(data, rel)
		}
	}
	if count > 0 {
		s.saveTags(data)
	}
	return count
}

// tagsOrDefault 保证 JSON 里是 [] 而不是 null（前端按数组渲染）。
func tagsOrDefault(all map[string][]string, rel string) []string {
	if tags, ok := all[rel]; ok && tags != nil {
		return tags
	}
	return []string{}
}

// SaveImageBytes 保存一张图并登记索引，返回相对路径与公开 URL。
//
// 文件名格式与 Python 一致：<unix秒>_<md5>.png，目录 YYYY/MM/DD。
// 存哪儿由 image_storage.mode 决定：local 只写本地，webdav 只传远端，
// both 两边都写。WebDAV 失败必须让整次保存失败 —— 静默退回本地会让
// webdav 模式的用户以为图已存到远端（本地磁盘照样涨满，重启换机就丢图）。
func (s *ImageService) SaveImageBytes(payload []byte, baseURL string) (rel string, url string, err error) {
	sum := md5.Sum(payload)
	now := time.Now()
	rel = fmt.Sprintf("%04d/%02d/%02d/%d_%s.png",
		now.Year(), int(now.Month()), now.Day(), now.Unix(), hex.EncodeToString(sum[:]))

	settings := s.cfg.ImageStorageSettings()
	mode := imageStorageMode(settings)

	storedLocal := false
	storedWebdav := false
	remoteURL := ""

	if mode == "local" || mode == "both" {
		full, pathErr := s.localImagePath(rel)
		if pathErr != nil {
			return "", "", pathErr
		}
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			return "", "", mkErr
		}
		if writeErr := os.WriteFile(full, payload, 0o644); writeErr != nil {
			return "", "", writeErr
		}
		storedLocal = true
	}

	if mode == "webdav" || mode == "both" {
		client := NewWebDAVClient(settings)
		if client.BaseURL == "" {
			if mode == "both" {
				// both 模式本地已有副本：远端没配好不该让请求失败。
				fmt.Fprintf(os.Stderr, "[image] webdav not configured, keeping local copy only: %s\n", rel)
			} else {
				return "", "", fmt.Errorf("WebDAV URL 不能为空")
			}
		} else {
			// publicBase 传空：索引里的 remote_url 与 Python 一致，记 WebDAV 上的真实地址，
			// 而不是对外域名（对外 URL 由 publicImageURL 现算）。
			putURL, putErr := client.Put(context.Background(), rel, payload, "")
			if putErr != nil {
				// both 模式下远端失败**不**让整个请求失败：本地副本已经写好了，
				// 因为远端抖动让用户拿不到图是不合理的。
				//
				// 但也绝不假装成功：索引如实标 storage=local / webdav=false，
				// 调用方看得出这张图没上远端，SyncToWebDAV 可事后补传。
				//
				// webdav 模式没有本地副本，失败必须报错 —— 那正是「静默降级」
				// 那个 bug 的反面（TestWebDAVOnlyModeStillFailsOnRemoteError 锁定）。
				if mode != "both" {
					return "", "", putErr
				}
				fmt.Fprintf(os.Stderr, "[image] webdav put failed, kept local copy only: %s: %v\n", rel, putErr)
			} else {
				remoteURL = putURL
				storedWebdav = true
			}
		}
	}

	storage := "local"
	switch {
	case storedLocal && storedWebdav:
		storage = "both"
	case storedWebdav:
		storage = "webdav"
	}

	item := map[string]any{
		"rel":        rel,
		"path":       rel,
		"name":       filepath.Base(rel),
		"date":       dateFromRel(rel, now),
		"size":       len(payload),
		"created_at": now.Format("2006-01-02 15:04:05"),
		"storage":    storage,
		"local":      storedLocal,
		"webdav":     storedWebdav,
		"remote_url": remoteURL,
	}
	// 宽高必须从 payload 解：webdav 模式下本地没有文件可读。
	if width, height, ok := imageDimensionsFromBytes(payload); ok {
		item["width"], item["height"] = width, height
	}

	s.indexMu.Lock()
	indexed := s.loadIndexLocked()
	indexed[rel] = item
	s.saveIndexLocked(indexed)
	s.indexMu.Unlock()

	return rel, s.publicImageURL(rel, baseURL), nil
}

func mimeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// removeEmptyDirs 自底向上删除空目录。
func removeEmptyDirs(root string) {
	var dirs []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() || path == root {
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})
	sort.SliceStable(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], string(os.PathSeparator)) > strings.Count(dirs[j], string(os.PathSeparator))
	})
	for _, dir := range dirs {
		_ = os.Remove(dir)
	}
}

// ImageError 携带状态码，便于 api 层映射到 404/422。
type ImageError struct {
	Status  int
	Message string
}

func (e *ImageError) Error() string { return e.Message }

// StatusOf 把 image 错误映射成 HTTP 状态。
func StatusOf(err error) int {
	var imageErr *ImageError
	if errors.As(err, &imageErr) {
		return imageErr.Status
	}
	if err == errImageNotFound {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// DiskFreeMB 返回图片目录所在磁盘的可用空间（MB）。
//
// 抽出来是为了让清理调度器能复用同一份 statfs 逻辑，
// 而不是在调度器里再写一遍平台相关的系统调用。
func (s *ImageService) DiskFreeMB() int {
	usage, err := diskUsage(s.cfg.ImagesDir())
	if err != nil {
		return 0
	}
	const mb = 1024 * 1024
	return int(usage.free / mb)
}
