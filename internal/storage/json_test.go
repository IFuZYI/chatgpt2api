package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 原子写：写完之后目录里不能留下临时文件，且内容可解析。
func TestSaveJSONValueIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	payload := []map[string]any{
		{"access_token": "tok-1", "type": "plus"},
		{"access_token": "tok-2", "type": "team"},
	}
	if err := saveJSONValue(path, payload); err != nil {
		t.Fatalf("saveJSONValue: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("saved file is not valid json: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("decoded %d entries, want 2", len(decoded))
	}

	// 临时文件必须已被 rename 掉（残留说明原子写没做对）。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "accounts.json" {
			t.Errorf("unexpected leftover file %q (temp file was not renamed/removed)", entry.Name())
		}
	}
}

// 覆盖写：第二次写入必须完整替换（不能出现新旧内容混在一起）。
func TestSaveJSONValueOverwritesCompletely(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	long := []map[string]any{}
	for i := 0; i < 50; i++ {
		long = append(long, map[string]any{"access_token": "token-with-a-long-name-" + string(rune('a'+i%26))})
	}
	if err := saveJSONValue(path, long); err != nil {
		t.Fatalf("first save: %v", err)
	}
	short := []map[string]any{{"access_token": "x"}}
	if err := saveJSONValue(path, short); err != nil {
		t.Fatalf("second save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("file is not valid json after overwrite: %v", err)
	}
	if len(decoded) != 1 || decoded[0]["access_token"] != "x" {
		t.Fatalf("decoded = %v, want exactly the short payload", decoded)
	}
}

// 目标目录不存在时要能自己建出来（与 os.WriteFile 版本的行为一致）。
func TestSaveJSONValueCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "accounts.json")
	if err := saveJSONValue(path, []map[string]any{{"access_token": "t"}}); err != nil {
		t.Fatalf("saveJSONValue: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	}
}

// 凭据文件必须是 0600。
//
// 历史：这里原本断言 0644，理由是「CreateTemp 默认 0600，必须显式改回来」——
// 但那等于把 accounts.json（含 access_token/refresh_token/password/
// totp_secret）设成同机任何用户可读。改为 0600 并锁住。
func TestSaveJSONValueKeepsFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	if err := saveJSONValue(path, []map[string]any{{"access_token": "t"}}); err != nil {
		t.Fatalf("saveJSONValue: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 644", mode)
	}
}
