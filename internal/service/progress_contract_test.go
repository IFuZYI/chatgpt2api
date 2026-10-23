package service

import (
	"encoding/json"
	"testing"
)

// 进度响应的字段形状是前端轮询契约，不是内部结构。
// 早期实现把 done 写成 int（已处理条数），前端 `if (p.done)` 于是在
// 第一个账号刷完时就判定「已完成」，停止轮询并报「刷新结果为空」——
// 账号页的刷新/恢复功能因此完全不可用。
func TestRefreshProgressContract(t *testing.T) {
	progress := &Progress{
		Total:        3,
		Processed:    1,
		StatusCounts: map[string]int{StatusNormal: 1},
		TotalQuota:   25,
	}
	got := progress.snapshot("refresh")

	// done 必须是 bool
	done, ok := got["done"].(bool)
	if !ok {
		t.Fatalf("done must be bool, got %T (%v)", got["done"], got["done"])
	}
	if done {
		t.Error("done should be false while still processing")
	}

	// processed 是已处理条数
	if got["processed"] != 1 {
		t.Errorf("processed = %v, want 1", got["processed"])
	}
	if got["total"] != 3 {
		t.Errorf("total = %v, want 3", got["total"])
	}

	// status_counts 四个键必须始终存在（前端直接索引）
	counts, ok := got["status_counts"].(map[string]any)
	if !ok {
		t.Fatalf("status_counts missing: %v", got["status_counts"])
	}
	for _, key := range []string{StatusNormal, StatusLimited, StatusAbnormal, StatusDisabled} {
		if _, exists := counts[key]; !exists {
			t.Errorf("status_counts missing key %q: %v", key, counts)
		}
	}

	if got["total_quota"] != 25 {
		t.Errorf("total_quota = %v, want 25", got["total_quota"])
	}
}

// done 只在 Finish 之后才为 true。
func TestRefreshProgressDoneOnlyAfterFinish(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-1", "type": "free", "status": StatusNormal, "quota": 25},
	})

	const id = "p1"
	svc.InitRefreshProgress(id, 1)

	// 处理中：done=false，processed 递增
	svc.UpdateRefreshProgress(id, "tok-1")
	mid := svc.GetRefreshProgress(id)
	if mid["done"] != false {
		t.Errorf("done should be false mid-flight, got %v", mid["done"])
	}
	if mid["processed"] != 1 {
		t.Errorf("processed = %v, want 1", mid["processed"])
	}
	// 状态分布与额度要跟着涨
	if counts, ok := mid["status_counts"].(map[string]any); ok {
		if counts[StatusNormal] != 1 {
			t.Errorf("status_counts[正常] = %v, want 1", counts[StatusNormal])
		}
	} else {
		t.Errorf("status_counts missing: %v", mid["status_counts"])
	}
	if mid["total_quota"] != 25 {
		t.Errorf("total_quota = %v, want 25", mid["total_quota"])
	}

	// 完成后：done=true
	svc.FinishRefreshProgress(id, map[string]any{"refreshed": 1}, "")
	done := svc.GetRefreshProgress(id)
	if done["done"] != true {
		t.Errorf("done should be true after finish, got %v", done["done"])
	}
	if done["result"] == nil {
		t.Error("result should be set after finish")
	}
}

// 重登进度带 results[]，每条含 token/name/status/error。
func TestReloginProgressContract(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "tok-1", "type": "free", "status": StatusNormal, "email": "a@b.c"},
	})

	const id = "r1"
	svc.InitReloginProgress(id, 1)

	initial := svc.GetReloginProgress(id)
	if initial["done"] != false {
		t.Errorf("done should be false initially, got %v", initial["done"])
	}
	if results, ok := initial["results"].([]any); !ok || len(results) != 0 {
		t.Errorf("results should be empty array initially, got %v", initial["results"])
	}

	svc.UpdateReloginProgress(id, "tok-1", "成功", "")
	mid := svc.GetReloginProgress(id)
	results, ok := mid["results"].([]any)
	if !ok {
		t.Fatalf("results missing: %v", mid["results"])
	}
	if len(results) != 1 {
		t.Fatalf("results length = %d, want 1", len(results))
	}
	entry, _ := results[0].(map[string]any)
	if entry["status"] != "成功" {
		t.Errorf("entry status = %v", entry["status"])
	}
	if entry["name"] != "a@b.c" {
		t.Errorf("entry name = %v, want a@b.c", entry["name"])
	}
	if mid["processed"] != 1 {
		t.Errorf("processed = %v, want 1", mid["processed"])
	}
}

// 进度响应必须能序列化成前端读得懂的 JSON（bool 不能被写成数字）。
func TestProgressJSONShape(t *testing.T) {
	progress := &Progress{Total: 1, Processed: 0, StatusCounts: map[string]int{}}
	payload, err := json.Marshal(progress.snapshot("refresh"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["done"].(bool); !ok {
		t.Errorf("done serialized as %T: %s", decoded["done"], payload)
	}
	if _, ok := decoded["processed"].(float64); !ok {
		t.Errorf("processed serialized as %T", decoded["processed"])
	}
}
