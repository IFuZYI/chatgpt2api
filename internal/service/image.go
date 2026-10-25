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

	"github.com/IFuZYI/chatgpt2api/internal/config"
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
	// tagsMu 保护 image_tags.json 的 load→mutate→save 全过程。
	// SetTags/DeleteTag/removeTags 都是读改写，无锁会互相覆盖。
	tagsMu sync.RWMutex

	// uploadWG 跟踪 both 模式的异步远端上传，供关闭时等待。
	uploadWG sync.WaitGroup
	// uploadClosed 置位后不再接受新的后台上传。
	uploadMu     sync.Mutex
	uploadClosed bool
}

// Close 等待在途的异步上传完成（进程退出前调用，避免半截上传）。
//
// 超时由调用方的 ctx 决定；这里只等 WaitGroup。
func (s *ImageService) Close(ctx context.Context) error {
	s.uploadMu.Lock()
	s.uploadClosed = true
	s.uploadMu.Unlock()

	done := make(chan struct{})
	go func() {
		s.uploadWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

// loadIndexLocked 读图片索引，损坏时回退到 .bak。
//
// 为什么要回退：webdav 模式下本地没有副本，**索引就是唯一记录**。
// 索引一旦损坏（进程写到一半被杀、磁盘满、手工编辑出错），原实现静默返回空表，
// 后果是整库消失且无修复路径：
//   - ListItems      → 0 条
//   - ReadImage      → 每张图 404（indexedOnWebDAV 门过不去）
//   - SyncToWebDAV   → uploaded:0（只 walk 本地文件，webdav 模式没有本地文件）
//
// saveIndexLocked 每次成功写入后留一份 .bak，这里解析失败就回退，
// 并把损坏的文件改名保留（便于事后排查，不直接覆盖掉）。
func (s *ImageService) loadIndexLocked() map[string]map[string]any {
	payload, err := os.ReadFile(s.indexPath())
	if err != nil {
		return map[string]map[string]any{}
	}
	var wrapper struct {
		Items map[string]map[string]any `json:"items"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		fmt.Printf("[image] 索引损坏，尝试从 .bak 恢复: %v\n", err)
		// 把损坏文件留档，避免下一次 save 直接把它覆盖掉。
		_ = os.Rename(s.indexPath(), s.indexPath()+".corrupt")
		if backup, readErr := os.ReadFile(s.indexPath() + ".bak"); readErr == nil {
			if json.Unmarshal(backup, &wrapper) == nil {
				fmt.Printf("[image] 已从 .bak 恢复 %d 条索引\n", len(wrapper.Items))
				out := make(map[string]map[string]any, len(wrapper.Items))
				for key, value := range wrapper.Items {
					if isImageRel(key) && value != nil {
						out[key] = value
					}
				}
				return out
			}
		}
		fmt.Printf("[image] .bak 也不可用，索引为空 —— webdav 模式下已存图片将无法读取\n")
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
	// 覆盖前把当前索引留一份 .bak：webdav 模式下索引是唯一记录，
	// 损坏即整库消失（loadIndexLocked 会从它回退）。
	if current, readErr := os.ReadFile(s.indexPath()); readErr == nil && len(current) > 0 {
		_ = os.WriteFile(s.indexPath()+".bak", current, 0o644)
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

// CleanupExpiredImages 按保留期清理，走 DeleteImages 以便同时处理
// 本地文件、WebDAV 远端对象、索引条目与缩略图。
//
// 为什么不能继续用 cfg.CleanupOldImages：它只 walk data/images 删本地文件，
// 三个模式全部有问题：
//
//	webdav → 本地没有副本，removed 恒为 0，**保留策略完全空转**，
//	         远端对象与索引条目永久累积
//	both   → 本地删了但索引条目留着且 webdav=true，ReadImage 仍从远端
//	         提供这张「已过期」的图，/api/images 里也还在
//	任意   → 缩略图不在此路径处理（CleanupThumbnails 在 webdav 模式自己也有 bug）
//
// 改成按日期调用 DeleteImages，四条清理路径一次走完。
func (s *ImageService) CleanupExpiredImages() int {
	days := s.cfg.ImageRetentionDays()
	if days <= 0 {
		return 0
	}
	// 保留期内的图不删。
	//
	// ListItems 的判据是 `day > endDate` 才过滤掉（image.go 里比较字符串），
	// 也就是 `day <= endDate` 的图会被返回并删除。所以 endDate 必须是
	// **保留期第一天的前一天**，否则保留期当天的图会被误删。
	//
	// 实测踩到：retention=30 时，恰好 30 天前的图被删了 ——
	// cutoff 落在今天同一时刻，那天的日期 == endDate，被当成「已过期」。
	//
	// 减去 1 天让 endDate = 保留期首日 - 1，语义变成「严格早于保留期首日才删」。
	cutoff := time.Now().AddDate(0, 0, -days-1)
	endDate := cutoff.Format("2006-01-02")
	// allMatching 必须是 true：DeleteImages 只在 allMatching 时按日期
	// 构建 targets，传 false 且 paths=nil 会让 targets 为空 ——
	// 循环体一次都不执行，removed 恒为 0（即清理完全空转）。
	result := s.DeleteImages(nil, "", endDate, true)
	removed := 0
	if n, ok := result["removed"].(int); ok {
		removed = n
	}
	return removed
}

// ListResponse 复刻 /api/images 的返回：items + 按日期分组。
func (s *ImageService) ListResponse(baseURL, startDate, endDate string) map[string]any {
	// 保留期清理走 CleanupExpiredImages（同时处理远端/索引/缩略图）。
	s.CleanupExpiredImages()
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

// webdavIndexedSet 返回「索引标记为 webdav 可用」的 rel 集合（一次解析）。
func (s *ImageService) webdavIndexedSet() map[string]bool {
	s.indexMu.Lock()
	indexed := s.loadIndexLocked()
	s.indexMu.Unlock()
	out := make(map[string]bool, len(indexed))
	for rel, item := range indexed {
		if truthy(item["webdav"]) {
			out[rel] = true
		}
	}
	return out
}

// CleanupThumbnails 删除源图已消失的缩略图，并清空目录。
func (s *ImageService) CleanupThumbnails() int {
	root := s.cfg.ImageThumbnailsDir()
	// 索引只读一次。
	//
	// 原来在 walk 里对每个缩略图调 indexedOnWebDAV(source)，它每次都
	// loadIndexLocked() —— 读文件 + 解析整个 JSON。n 个缩略图就是 n 次全量解析，
	// 实测 400 张 1.70s / 800 张 6.14s（超线性），而本函数由 ListResponse
	// 调用，也就是每次打开管理页都跑。
	webdavSet := s.webdavIndexedSet()
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
		// webdav 模式下本地永远没有源文件，只看「本地存在」会把**每一张**
		// 缩略图都删掉 —— 而 ListResponse 每次调用都触发本函数，于是每次
		// 刷新管理页都会清空缩略图，EnsureThumbnail 再从远端整图重新生成。
		// 用与 ReadImage 相同的门（indexedOnWebDAV）判断源是否仍在。
		if webdavSet[source] {
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
		// 远端 404 说明索引说谎了（对象已被生命周期策略/手工清理删掉）。
		// 映射成 errImageNotFound（→ HTTP 404）而不是把原始 WebDAV 错误
		// 冒泡成 500 —— 前者是「这张图没了」，后者是「服务器出错」，语义不同。
		// 顺手把索引里的 webdav 标记清掉，让 ListItems 下次能回收这个僵尸条目。
		if isWebDAVNotFound(remoteErr) {
			s.clearWebDAVFlag(safe)
			return nil, "", errImageNotFound
		}
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

// isWebDAVNotFound 判断远端错误是否是 404。
func isWebDAVNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "WebDAV GET failed: HTTP 404")
}

// clearWebDAVFlag 把索引条目的 webdav 标记清掉（对象已不存在）。
func (s *ImageService) clearWebDAVFlag(rel string) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	indexed := s.loadIndexLocked()
	item, exists := indexed[rel]
	if !exists {
		return
	}
	if !truthy(item["webdav"]) {
		return
	}
	item["webdav"] = false
	item["remote_url"] = ""
	if !truthy(item["local"]) {
		item["storage"] = "local"
	}
	indexed[rel] = item
	s.saveIndexLocked(indexed)
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
	// 只对**真的删掉了**的条目移除索引。
	//
	// 原实现对每个请求的 rel 无条件 delete(indexed, rel)，两个后果：
	//
	//   1. 远端删除失败（webdav 模式 500）：removed:0 但索引条目没了 ——
	//      远端对象从此永久不可删，因为 deleteRemoteImage 要求
	//      indexedOnWebDAV(rel)，而索引里已经没有它了。
	//   2. 本地 os.Remove 失败（chattr/EBUSY/NFS）：索引条目没了但文件还在，
	//      下一次 ListItems 扫盘会把它当成新条目**加回来** —— 删除静默回滚。
	//
	// 所以把「删成功的 rel」收集起来，索引只对它们生效。
	deletedRels := make([]string, 0, len(targets))
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
			deletedRels = append(deletedRels, rel)
		}
		if thumb, err := s.ThumbnailPath(rel); err == nil && fileExists(thumb) {
			_ = os.Remove(thumb)
		}
		s.removeTags(rel)
	}

	if len(deletedRels) > 0 {
		s.indexMu.Lock()
		indexed := s.loadIndexLocked()
		changed := false
		for _, rel := range deletedRels {
			if _, exists := indexed[rel]; exists {
				delete(indexed, rel)
				changed = true
			}
		}
		if changed {
			s.saveIndexLocked(indexed)
		}
		s.indexMu.Unlock()
	}

	removeEmptyDirs(s.cfg.ImagesDir())
	removeEmptyDirs(s.cfg.ImageThumbnailsDir())
	return map[string]any{"removed": removed}
}

// ---- 标签 ----

// loadTags 读标签表。
//
// 必须持 tagsMu.RLock：写方（saveTags）在 tagsMu 下做整文件重写，
// 读方此前完全无锁，会读到「文件被截断但还没写完」的中间态。
// 实测并发 SetTags 期间 4000 次读里有 197 次读到空表 ——
// 管理页会间歇性地把所有图片渲染成「无标签」。
//
// 用 defer RUnlock 而不是在读之前 Unlock：读写要覆盖整个 ReadFile，
// 否则仍有窗口。
func (s *ImageService) loadTags() map[string][]string {
	s.tagsMu.RLock()
	defer s.tagsMu.RUnlock()
	return s.loadTagsLocked()
}

// loadTagsLocked 是不加锁的读，供已持锁的写方复用（避免自死锁）。
func (s *ImageService) loadTagsLocked() map[string][]string {
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

// saveTags 原子写标签表（tmp + rename）。
//
// 原来是 os.WriteFile（截断后重写）：进程在写到一半时崩溃/被杀，
// 文件就是半截 JSON；下次 loadTags 解析失败返回空表，
// 再下一次 SetTags 会以空表为基底写回 —— **全部标签丢失**。
// 索引与任务文件早就是 tmp+rename，只有这里漏了。
func (s *ImageService) saveTags(data map[string][]string) {
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	payload = append(payload, '\n')
	path := s.tagsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
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
	s.tagsMu.Lock()
	defer s.tagsMu.Unlock()

	data := s.loadTagsLocked()
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
	s.tagsMu.Lock()
	defer s.tagsMu.Unlock()

	data := s.loadTagsLocked()
	if _, exists := data[rel]; !exists {
		return
	}
	delete(data, rel)
	s.saveTags(data)
}

// DeleteTag 从所有图片里移除某标签，返回受影响图片数。
func (s *ImageService) DeleteTag(tag string) int {
	s.tagsMu.Lock()
	defer s.tagsMu.Unlock()

	data := s.loadTagsLocked()
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
// both 两边都写。
//
// WebDAV 失败时的行为按模式区分（ADR-0011 / ADR-0012）：
//   - webdav：整次保存失败。静默退回本地会让用户以为图已在远端，
//     而本地磁盘照样涨满，重启换机就丢图。
//   - both：降级为本地并返回成功，索引如实标记 storage=local / webdav=false。
//     远端上传转异步（ADR-0012），不阻塞生图响应。
//
// 早期注释写的是「WebDAV 失败必须让整次保存失败」，那只对 webdav 模式成立，
// 已被 ADR-0011/0012 取代。
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

	// webdav 模式：同步上传（本地没有副本，必须知道远端成不成）。
	if mode == "webdav" {
		client := NewWebDAVClient(settings)
		if client.BaseURL == "" {
			return "", "", fmt.Errorf("WebDAV URL 不能为空")
		}
		putURL, putErr := client.Put(context.Background(), rel, payload, "")
		if putErr != nil {
			return "", "", putErr
		}
		remoteURL = putURL
		storedWebdav = true
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

	// both 模式：远端上传异步进行，生图不等它。
	//
	// 理由：WebDAV 的 PROPFIND/PUT 实测要 11-20 秒。同步等会把生图
	// 从「上游耗时」拖成「上游 + 远端」，而远端慢不该影响用户拿图。
	//
	// 索引先标 storage=local（如实），上传成功后改 both/webdav=true。
	// 读取路径本来就是本地优先（ReadImage 先读本地文件），所以这个
	// 中间态对用户无感；远端失败时索引保持 local，可事后 SyncToWebDAV 补传。
	if mode == "both" {
		s.startBackgroundUpload(rel, payload, settings)
	}

	return rel, s.publicImageURL(rel, baseURL), nil
}

// startBackgroundUpload 在后台把一张图传到 WebDAV，成功后更新索引。
func (s *ImageService) startBackgroundUpload(rel string, payload []byte, settings map[string]any) {
	s.uploadMu.Lock()
	if s.uploadClosed {
		s.uploadMu.Unlock()
		return
	}
	s.uploadWG.Add(1)
	s.uploadMu.Unlock()

	go func() {
		defer s.uploadWG.Done()

		client := NewWebDAVClient(settings)
		if client.BaseURL == "" {
			fmt.Fprintf(os.Stderr, "[image] webdav not configured, keeping local copy only: %s\n", rel)
			return
		}

		putURL, putErr := client.Put(context.Background(), rel, payload, "")
		if putErr != nil {
			fmt.Fprintf(os.Stderr, "[image] webdav put failed, kept local copy only: %s: %v\n", rel, putErr)
			return
		}

		s.indexMu.Lock()
		indexed := s.loadIndexLocked()
		item, exists := indexed[rel]
		if exists {
			item["storage"] = "both"
			item["webdav"] = true
			item["remote_url"] = putURL
			indexed[rel] = item
			s.saveIndexLocked(indexed)
		}
		s.indexMu.Unlock()
	}()
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
// removeEmptyDirsMinAge 只删除「空且够旧」的目录。
//
// 为什么需要年龄门槛：SaveImageBytes 的流程是 MkdirAll(dir) → WriteFile。
// 中间那一瞬目录是空的；如果清理路径此刻把它删掉，随后的 WriteFile 会
// ENOENT —— 用户看到 502，而生图额度已经消耗。
//
// 实测：4 个并发 saver × 300 张 vs 清理循环 → 73 次保存失败，
// 首个错误 `open .../2026/10/25/xxx.png: no such file or directory`。
// 清理调度器在磁盘紧张时每 30 分钟跑一次，正是实例最忙的时候。
//
// 用 2 分钟门槛：任何活跃写入路径的目录都不会那么旧；而真正的空目录
// （删除图片后留下的）迟早会被下一次清理收掉。
const removeEmptyDirsMinAge = 2 * time.Minute

func removeEmptyDirs(root string) {
	now := time.Now()
	var dirs []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() || path == root {
			return nil
		}
		// 太新的目录可能是并发保存刚创建的，跳过。
		if now.Sub(info.ModTime()) < removeEmptyDirsMinAge {
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
