package service

import (
	"context"
	"errors"
	"testing"
)

// 回归点 1：KeepaliveRefreshTokens 传给回调的必须是 refresh_token，
// 不是 access_token。签名是 RefreshTokenFunc(refreshToken)，名字与语义都在说这件事。
func TestKeepaliveRefreshPassesRefreshTokenNotAccessToken(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-1", "type": "free", "status": StatusNormal},
	})

	var got string
	refresh := func(ctx context.Context, refreshToken string) (string, error) {
		got = refreshToken
		return "at-new", nil
	}
	svc.KeepaliveRefreshTokens(context.Background(), []string{"at-1"}, refresh)

	if got != "rt-1" {
		t.Fatalf("回调收到的 token = %q，应为 refresh_token %q（不是 access_token）", got, "rt-1")
	}
}

// 回归点 2：换到新 access_token 后必须回写账号，否则这次 keepalive 白做。
func TestKeepaliveRefreshWritesBackNewAccessToken(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-1", "type": "free", "status": StatusNormal},
	})

	refresh := func(ctx context.Context, refreshToken string) (string, error) {
		return "at-2", nil
	}
	result := svc.KeepaliveRefreshTokens(context.Background(), []string{"at-1"}, refresh)

	if n := intValue(result["refreshed"]); n != 1 {
		t.Fatalf("refreshed = %d，应为 1", n)
	}
	found := false
	for _, acc := range svc.ListAccounts() {
		if stringValue(acc["access_token"]) == "at-2" {
			found = true
			if rt := stringValue(acc["refresh_token"]); rt != "rt-1" {
				t.Errorf("新账号的 refresh_token = %q，应保留 %q", rt, "rt-1")
			}
		}
	}
	if !found {
		t.Fatalf("新 access_token 没有回写到号池，keepalive 白做")
	}
}

// 回归点 3：回调失败要记进 errors，且不能把账号改坏。
func TestKeepaliveRefreshRecordsErrorAndKeepsAccount(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-1", "type": "free", "status": StatusNormal},
	})

	refresh := func(ctx context.Context, refreshToken string) (string, error) {
		return "", errors.New("boom")
	}
	result := svc.KeepaliveRefreshTokens(context.Background(), []string{"at-1"}, refresh)

	if n := intValue(result["refreshed"]); n != 0 {
		t.Errorf("refreshed = %d，应为 0", n)
	}
	if errs, ok := result["errors"].([]any); !ok || len(errs) != 1 {
		t.Errorf("errors = %v，应有 1 条", result["errors"])
	}
	for _, acc := range svc.ListAccounts() {
		if stringValue(acc["access_token"]) == "at-1" {
			return
		}
	}
	t.Fatalf("失败时不应动账号，但 at-1 不见了")
}
