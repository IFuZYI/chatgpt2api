package service

import (
	"fmt"
	"sync"
	"testing"
)

// TestSetTagsConcurrentNoLostUpdate 是并发丢更新的回归用例。
//
// image_tags.json 是「读文件 → 改 map → 写文件」的读改写。没有锁时并发
// SetTags 会互相覆盖：50 个并发调用实测只留下 0~1 条。加 tagsMu 后必须
// 全部保留。
func TestSetTagsConcurrentNoLostUpdate(t *testing.T) {
	svc, _ := newImageStorageFixture(t, "local", "")

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rel := fmt.Sprintf("2026/10/24/img_%03d.png", i)
			svc.SetTags(rel, []string{fmt.Sprintf("tag%d", i)})
		}(i)
	}
	wg.Wait()

	if got := len(svc.loadTags()); got != n {
		t.Fatalf("并发 SetTags %d 次只保留了 %d 条，丢更新 %d 条", n, got, n-got)
	}
}

// TestDeleteTagConcurrentNoLostUpdate 覆盖 DeleteTag 的读改写路径。
func TestDeleteTagConcurrentNoLostUpdate(t *testing.T) {
	svc, _ := newImageStorageFixture(t, "local", "")

	const n = 30
	for i := 0; i < n; i++ {
		svc.SetTags(fmt.Sprintf("2026/10/24/img_%03d.png", i), []string{"shared", fmt.Sprintf("keep%d", i)})
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			svc.DeleteTag(fmt.Sprintf("keep%d", i))
		}(i)
	}
	wg.Wait()

	data := svc.loadTags()
	if len(data) != n {
		t.Fatalf("DeleteTag 后应保留 %d 个条目，实际 %d", n, len(data))
	}
	for rel, tags := range data {
		if len(tags) != 1 || tags[0] != "shared" {
			t.Fatalf("%s 的标签应为 [shared]，实际 %v", rel, tags)
		}
	}
}
