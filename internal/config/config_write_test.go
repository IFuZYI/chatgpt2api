package config

import (
	"os"
	"path/filepath"
	"testing"
)

// config 写入必须在「目录不可写、但文件本身可写」时也能成功。
//
// 这是 Docker 单文件 bind mount 的真实形态：
//   - /app 是镜像内的目录，以 nonroot(65532) 运行 → 不可写，无法创建 .tmp
//   - /app/config.json 是宿主 bind mount 的文件 → 若宿主权限允许，文件可写
//
// 当前实现先试 os.WriteFile(path+".tmp")，失败后回退就地写 ——
// 逻辑本身对。但如果 .tmp 创建失败的原因是**目录不可写**，
// 回退也依赖文件本身可写。这条测试锁住「回退真的会执行」。
func TestWriteFileAtomicFallsBackWhenDirectoryReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视目录权限，无法构造该场景")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// 目录设为只读（不能创建 .tmp），但文件保持可写
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := writeFileAtomic(path, []byte(`{"a":2}`)); err != nil {
		t.Fatalf("目录只读时应回退到就地写，实际报错: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != `{"a":2}` {
		t.Errorf("内容未写入: %q", got)
	}
}

// 原子写成功后不应留下 .tmp 残留。
func TestWriteFileAtomicLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := writeFileAtomic(path, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error(".tmp 残留")
	}
}
