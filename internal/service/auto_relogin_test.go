package service

import (
	"context"
	"errors"
	"testing"

	"github.com/IFuZYI/chatgpt2api/internal/upstream"
)

// 开关打开 + 执行体已注入：刷新发现失效账号后应自动重登。
func TestRefreshAutoReloginTriggersOnInvalidToken(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-1", "email": "a@example.test",
			"password": "pw", "type": "free", "status": StatusNormal},
	})
	// 打开开关（config.Store 需要落盘才生效，这里直接调 setter 路径）
	if _, err := cfg.Update(map[string]any{"auto_relogin_after_refresh": true}); err != nil {
		t.Fatalf("update config: %v", err)
	}

	called := 0
	svc.SetAutoRelogin(func(ctx context.Context, email, password, totp string) (map[string]string, error) {
		called++
		return map[string]string{"access_token": "at-2", "refresh_token": "rt-2", "id_token": "id-2"}, nil
	})

	// verify 报 401 → isTokenInvalidFailure 为真
	verify := func(ctx context.Context, token, event string) (Account, error) {
		return nil, upstream.NewStatusError(401, nil, "token invalidated")
	}
	result := svc.RefreshAccounts(context.Background(), []string{"at-1"}, "", verify)

	if called == 0 {
		t.Fatalf("开关打开且执行体已注入，但自动重登没有被调用；result=%v", result)
	}
	if n := intValue(result["relogined"]); n != 1 {
		t.Errorf("relogined = %d，应为 1", n)
	}
}

// 开关关闭时绝不自动重登（避免无谓的密码登录与风控风险）。
func TestRefreshAutoReloginRespectsSwitchOff(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-1", "email": "a@example.test",
			"password": "pw", "type": "free", "status": StatusNormal},
	})

	called := 0
	svc.SetAutoRelogin(func(ctx context.Context, email, password, totp string) (map[string]string, error) {
		called++
		return map[string]string{"access_token": "at-2"}, nil
	})

	verify := func(ctx context.Context, token, event string) (Account, error) {
		return nil, upstream.NewStatusError(401, nil, "token invalidated")
	}
	svc.RefreshAccounts(context.Background(), []string{"at-1"}, "", verify)

	if called != 0 {
		t.Fatalf("开关关闭时不应自动重登，但被调用了 %d 次", called)
	}
}

// 429 限流不算失效，不能触发自动重登（否则上游抖动会批量密码登录）。
func TestRefreshAutoReloginIgnoresRateLimit(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-1", "email": "a@example.test",
			"password": "pw", "type": "free", "status": StatusNormal},
	})
	if _, err := cfg.Update(map[string]any{"auto_relogin_after_refresh": true}); err != nil {
		t.Fatalf("update config: %v", err)
	}

	called := 0
	svc.SetAutoRelogin(func(ctx context.Context, email, password, totp string) (map[string]string, error) {
		called++
		return nil, errors.New("should not be called")
	})

	verify := func(ctx context.Context, token, event string) (Account, error) {
		return nil, upstream.NewStatusError(429, nil, "rate limited")
	}
	svc.RefreshAccounts(context.Background(), []string{"at-1"}, "", verify)

	if called != 0 {
		t.Fatalf("429 不应触发自动重登，但被调用了 %d 次", called)
	}
}

// 未注入执行体时静默跳过，不能 panic。
func TestRefreshAutoReloginWithoutImplementationDoesNotPanic(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-1", "email": "a@example.test",
			"password": "pw", "type": "free", "status": StatusNormal},
	})
	if _, err := cfg.Update(map[string]any{"auto_relogin_after_refresh": true}); err != nil {
		t.Fatalf("update config: %v", err)
	}

	verify := func(ctx context.Context, token, event string) (Account, error) {
		return nil, upstream.NewStatusError(401, nil, "token invalidated")
	}
	result := svc.RefreshAccounts(context.Background(), []string{"at-1"}, "", verify)
	if n := intValue(result["relogined"]); n != 0 {
		t.Errorf("未注入执行体时 relogined 应为 0，实际 %d", n)
	}
}
