package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// webdavFake 模拟一个最小 WebDAV 服务器。
type webdavFake struct {
	mu    sync.Mutex
	files map[string][]byte
	dirs  map[string]bool
	auth  string
}

func newWebDAVFake() *webdavFake {
	return &webdavFake{files: map[string][]byte{}, dirs: map[string]bool{}}
}

func (f *webdavFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.auth != "" && r.Header.Get("Authorization") != f.auth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		path := r.URL.Path

		switch r.Method {
		case "MKCOL":
			f.mu.Lock()
			if f.dirs[path] {
				f.mu.Unlock()
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			f.dirs[path] = true
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			body := make([]byte, r.ContentLength)
			r.Body.Read(body)
			f.mu.Lock()
			f.files[path] = body
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			f.mu.Lock()
			payload, ok := f.files[path]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(payload)
		case http.MethodDelete:
			f.mu.Lock()
			_, ok := f.files[path]
			delete(f.files, path)
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "PROPFIND":
			w.WriteHeader(207)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

func TestWebDAVPutGetDelete(t *testing.T) {
	fake := newWebDAVFake()
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	client := NewWebDAVClient(map[string]any{
		"webdav_url":       server.URL,
		"webdav_username":  "",
		"webdav_password":  "",
		"webdav_root_path": "chatgpt2api/images",
	})
	ctx := context.Background()

	url, err := client.Put(ctx, "2026/10/23/a.png", []byte("PNGDATA"), "")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !strings.Contains(url, "2026/10/23/a.png") {
		t.Errorf("url = %q", url)
	}

	payload, err := client.Get(ctx, "2026/10/23/a.png")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(payload) != "PNGDATA" {
		t.Errorf("payload = %q", payload)
	}

	removed, err := client.Delete(ctx, "2026/10/23/a.png")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !removed {
		t.Error("delete should report removed=true")
	}

	// 再删一次：404 视为成功但 removed=false
	removed, err = client.Delete(ctx, "2026/10/23/a.png")
	if err != nil {
		t.Fatalf("second delete should not error: %v", err)
	}
	if removed {
		t.Error("second delete should report removed=false")
	}
}

func TestWebDAVBasicAuth(t *testing.T) {
	fake := newWebDAVFake()
	fake.auth = "Basic " + basicAuthToken("user", "pass")
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	client := NewWebDAVClient(map[string]any{
		"webdav_url":       server.URL,
		"webdav_username":  "user",
		"webdav_password":  "pass",
		"webdav_root_path": "root",
	})
	if _, err := client.Put(context.Background(), "x.png", []byte("data"), ""); err != nil {
		t.Fatalf("authenticated put should succeed: %v", err)
	}

	wrong := NewWebDAVClient(map[string]any{
		"webdav_url":       server.URL,
		"webdav_username":  "user",
		"webdav_password":  "wrong",
		"webdav_root_path": "root",
	})
	if _, err := wrong.Put(context.Background(), "y.png", []byte("data"), ""); err == nil {
		t.Fatal("wrong password should fail")
	}
}

func TestTestWebDAV(t *testing.T) {
	fake := newWebDAVFake()
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	result, err := TestWebDAV(context.Background(), map[string]any{
		"webdav_url":       server.URL,
		"webdav_root_path": "root",
	})
	if err != nil {
		t.Fatalf("TestWebDAV: %v", err)
	}
	if result["ok"] != true {
		t.Errorf("ok = %v", result["ok"])
	}
	if result["status"] != 207 {
		t.Errorf("status = %v, want 207", result["status"])
	}
}

func TestTestWebDAVRequiresURL(t *testing.T) {
	if _, err := TestWebDAV(context.Background(), map[string]any{}); err == nil {
		t.Fatal("empty url should fail")
	}
}

// public_base_url 优先于 WebDAV 自身地址。
func TestWebDAVPublicBaseURL(t *testing.T) {
	fake := newWebDAVFake()
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	client := NewWebDAVClient(map[string]any{
		"webdav_url":       server.URL,
		"webdav_root_path": "root",
	})
	url, err := client.Put(context.Background(), "a/b.png", []byte("x"), "https://cdn.example.com")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if url != "https://cdn.example.com/a/b.png" {
		t.Errorf("public url = %q", url)
	}
}

func basicAuthToken(user, pass string) string {
	// base64("user:pass")
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	input := []byte(user + ":" + pass)
	var out []byte
	for i := 0; i < len(input); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], input[i:])
		value := uint32(chunk[0])<<16 | uint32(chunk[1])<<8 | uint32(chunk[2])
		out = append(out, table[value>>18&0x3f], table[value>>12&0x3f])
		if n > 1 {
			out = append(out, table[value>>6&0x3f])
		} else {
			out = append(out, '=')
		}
		if n > 2 {
			out = append(out, table[value&0x3f])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}
