package service

import (
	"os/exec"
	"testing"
)

// GG. os.Remove 失败被静默忽略，但 removed/freed 照加 → 报告 done=true 实际没删。
func TestZZDeleteToTargetFalseSuccess(t *testing.T) {
	svc, _ := newImageStorageFixture(t, "local", "")
	rel, _, err := svc.SaveImageBytes(testPNG(t, 64, 64), "")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	full, _ := svc.localImagePath(rel)
	if err := exec.Command("chattr", "+i", full).Run(); err != nil {
		t.Skipf("chattr unavailable: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("chattr", "-i", full).Run() })

	free := svc.DiskFreeMB()
	res := svc.DeleteToTarget(free+1, false)
	t.Logf("DeleteToTarget(target=%d, free=%d) => %v", free+1, free, res)

	stillThere := fileExists(full)
	t.Logf("file still on disk: %v", stillThere)
	if stillThere && res["removed"].(int) > 0 {
		t.Fatalf("reported removed=%v / freed=%v MB / done=%v, but the file is still on disk: "+
			"image_ops.go:116 discards os.Remove's error while still counting freed bytes",
			res["removed"], res["freed_mb"], res["done"])
	}
}
