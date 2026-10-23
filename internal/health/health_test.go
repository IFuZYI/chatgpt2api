package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Check 对 /health 发一次 GET，2xx 视为健康。
//
// 存在理由：distroless 镜像里没有 curl/wget，Docker healthcheck 只能
// 用二进制自己。没有它就只能靠 restart 策略，容器卡死不会被发现。
func TestCheckReportsHealthyOn200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("probed %q, want /health", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := Check(context.Background(), server.URL, time.Second); err != nil {
		t.Errorf("Check = %v, want nil", err)
	}
}

// 非 2xx 必须报错（容器不健康）。
func TestCheckFailsOn500(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	if err := Check(context.Background(), server.URL, time.Second); err == nil {
		t.Error("Check = nil on 500, want error")
	}
}

// 连不上必须报错（而不是挂住）。
func TestCheckFailsOnConnectionError(t *testing.T) {
	if err := Check(context.Background(), "http://127.0.0.1:1", 500*time.Millisecond); err == nil {
		t.Error("Check = nil on unreachable, want error")
	}
}

// 超时必须按时返回（healthcheck 有 timeout 限制）。
func TestCheckRespectsTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	start := time.Now()
	err := Check(context.Background(), server.URL, 200*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("Check = nil on timeout, want error")
	}
	if elapsed > time.Second {
		t.Errorf("Check took %v, want < 1s (must respect the timeout)", elapsed)
	}
}
