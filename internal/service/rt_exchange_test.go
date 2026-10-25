package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 回归点 1：KeepaliveRefreshTokens 传给回调的必须是 refresh_token，
// 不是 access_token。签名是 RefreshTokenFunc(refreshToken)，名字与语义都在说这件事。
func TestKeepaliveRefreshPassesRefreshTokenNotAccessToken(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-1", "type": "free", "status": StatusNormal},
	})

	var got string
	refresh := func(ctx context.Context, refreshToken string) (map[string]string, error) {
		got = refreshToken
		return map[string]string{"access_token": "at-new"}, nil
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

	refresh := func(ctx context.Context, refreshToken string) (map[string]string, error) {
		return map[string]string{"access_token": "at-2"}, nil
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

	refresh := func(ctx context.Context, refreshToken string) (map[string]string, error) {
		return nil, errors.New("boom")
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

// 回归点 4：轮换后的新 refresh_token 必须被写回账号。
//
// 上游每次换 AT 都会返回一个新 RT（实测 196 → 211 字符）。若只写 access_token，
// 账号会一直用旧 RT 续期 —— 等于放弃上游给的新凭据。
func TestKeepaliveWritesBackRotatedRefreshToken(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-old", "type": "free", "status": StatusNormal},
	})

	refresh := func(ctx context.Context, refreshToken string) (map[string]string, error) {
		return map[string]string{
			"access_token":  "at-2",
			"refresh_token": "rt-rotated",
			"id_token":      "id-2",
		}, nil
	}
	svc.KeepaliveRefreshTokens(context.Background(), []string{"at-1"}, refresh)

	var found bool
	for _, acc := range svc.ListAccounts() {
		if stringValue(acc["access_token"]) != "at-2" {
			continue
		}
		found = true
		if rt := stringValue(acc["refresh_token"]); rt != "rt-rotated" {
			t.Errorf("refresh_token = %q，应为轮换后的 %q", rt, "rt-rotated")
		}
		if id := stringValue(acc["id_token"]); id != "id-2" {
			t.Errorf("id_token = %q，应为 %q", id, "id-2")
		}
		if anchor := stringValue(acc["last_token_refresh_at"]); anchor == "" {
			t.Error("last_token_refresh_at 未写入，keepalive 锚点会退回到 AT 的 iat")
		}
	}
	if !found {
		t.Fatal("新 access_token 未回写")
	}
}

// 上游没回传新 RT 时必须保留旧值（实测旧 RT 仍可用，覆盖它会丢凭据）。
func TestKeepaliveKeepsOldRefreshTokenWhenNotRotated(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-keep-me", "type": "free", "status": StatusNormal},
	})

	refresh := func(ctx context.Context, refreshToken string) (map[string]string, error) {
		return map[string]string{"access_token": "at-2"}, nil // 无 refresh_token
	}
	svc.KeepaliveRefreshTokens(context.Background(), []string{"at-1"}, refresh)

	for _, acc := range svc.ListAccounts() {
		if stringValue(acc["access_token"]) != "at-2" {
			continue
		}
		if rt := stringValue(acc["refresh_token"]); rt != "rt-keep-me" {
			t.Errorf("refresh_token = %q，上游未回传时应保留旧值", rt)
		}
		return
	}
	t.Fatal("未找到新账号")
}

// 回归点 5：keepalive 失败必须写 last_token_refresh_error_at。
//
// 没有它 recentRefreshTokenKeepaliveErrorLocked 恒定返回 false，
// 6 小时退避永不生效 —— RT 失效的账号会被每轮 tick（5 分钟）重试。
func TestKeepaliveFailureRecordsErrorTimestamp(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-1", "type": "free", "status": StatusNormal},
	})

	refresh := func(ctx context.Context, refreshToken string) (map[string]string, error) {
		return nil, errors.New("invalid_grant")
	}
	svc.KeepaliveRefreshTokens(context.Background(), []string{"at-1"}, refresh)

	for _, acc := range svc.ListAccounts() {
		if stringValue(acc["access_token"]) != "at-1" {
			continue
		}
		if at := stringValue(acc["last_token_refresh_error_at"]); at == "" {
			t.Error("失败时未写 last_token_refresh_error_at，6 小时退避不会生效")
		}
		if reason := stringValue(acc["last_token_refresh_error"]); reason == "" {
			t.Error("失败原因未记录")
		}
		// 退避应即刻生效：刚失败过 => 不应被再次挑进 keepalive 列表
		if got := svc.ListRefreshTokenKeepaliveTokens(); len(got) != 0 {
			t.Errorf("刚失败的账号仍被挑入 keepalive: %v", got)
		}
		return
	}
	t.Fatal("未找到账号")
}

// 回归点 6：RT 被上游拒绝时必须立即触发密码重登，而不是等 AT 过期。
//
// 之前只记日志：RT 死了以后 AT 还能用最长 10 天，这期间账号看着「正常」
// 但续期能力已经没了。等 AT 过期才发现，用户会看到账号集体变「异常」。
func TestKeepaliveTriggersReloginWhenRTRejected(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-dead", "email": "a@x.test",
			"password": "pw", "type": "free", "status": StatusNormal},
	})
	if _, err := cfg.Update(map[string]any{
		"auto_relogin_after_refresh":   true,
		"auto_remove_invalid_accounts": false,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	reloginCalled := 0
	svc.SetAutoRelogin(func(ctx context.Context, email, password, totp string) (map[string]string, error) {
		reloginCalled++
		return map[string]string{"access_token": "at-recovered", "refresh_token": "rt-new"}, nil
	})

	refresh := func(ctx context.Context, refreshToken string) (map[string]string, error) {
		return nil, &RefreshTokenRejectedError{StatusCode: 401, Detail: "invalid_grant"}
	}
	result := svc.KeepaliveRefreshTokens(context.Background(), []string{"at-1"}, refresh)

	if reloginCalled != 1 {
		t.Fatalf("RT 被拒时应调用一次密码重登，实际 %d 次", reloginCalled)
	}
	if n := intValue(result["relogined"]); n != 1 {
		t.Errorf("relogined = %d，应为 1", n)
	}
	for _, acc := range svc.ListAccounts() {
		if stringValue(acc["access_token"]) == "at-recovered" {
			return // 恢复成功
		}
	}
	t.Fatal("账号未被重登恢复")
}

// 网络故障**不应**触发重登 —— 否则一次上游抖动会批量跑密码登录。
func TestKeepaliveDoesNotReloginOnTransientError(t *testing.T) {
	svc, cfg := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-1", "email": "a@x.test",
			"password": "pw", "type": "free", "status": StatusNormal},
	})
	if _, err := cfg.Update(map[string]any{"auto_relogin_after_refresh": true}); err != nil {
		t.Fatalf("update: %v", err)
	}

	reloginCalled := 0
	svc.SetAutoRelogin(func(ctx context.Context, email, password, totp string) (map[string]string, error) {
		reloginCalled++
		return nil, nil
	})

	// 5xx / 网络错误 → 非拒绝
	for _, e := range []error{
		errors.New("oauth_refresh_http_503: upstream unavailable"),
		errors.New("refresh_token 换 access_token 网络异常: dial timeout"),
	} {
		svc.KeepaliveRefreshTokens(context.Background(), []string{"at-1"},
			func(ctx context.Context, rt string) (map[string]string, error) { return nil, e })
	}
	if reloginCalled != 0 {
		t.Fatalf("临时故障不应触发重登，实际调用 %d 次", reloginCalled)
	}
}

// 回归点 7：重登成功后必须重置 keepalive 锚点并清掉旧错误。
//
// 不重置的后果：锚点停在旧值 => 下一轮 tick 立刻又挑中这个账号，
// 刚花完一次密码登录又被拉去换 AT；旧错误留着会让面板显示过时信息。
func TestReloginResetsKeepaliveState(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-old", "refresh_token": "rt-dead", "email": "a@x.test",
			"password": "pw", "type": "free", "status": StatusAbnormal,
			"last_token_refresh_at":       "2020-01-01 00:00:00",
			"last_token_refresh_error":    "refresh_token_reused",
			"last_token_refresh_error_at": "2020-01-01 00:00:00"},
	})

	login := func(ctx context.Context, email, password, totp string) (map[string]string, error) {
		return map[string]string{
			"access_token":  "at-new",
			"refresh_token": "rt-fresh",
			"id_token":      "id-new",
		}, nil
	}
	res := svc.ReloginAccounts(context.Background(), []string{"at-old"}, "", login)
	if n := intValue(res["relogined"]); n != 1 {
		t.Fatalf("relogined = %d，应为 1", n)
	}

	var checked bool
	for _, acc := range svc.ListAccounts() {
		if stringValue(acc["access_token"]) != "at-new" {
			continue
		}
		checked = true
		if got := stringValue(acc["last_token_refresh_at"]); got == "2020-01-01 00:00:00" || got == "" {
			t.Errorf("keepalive 锚点未重置，仍为 %q", got)
		}
		if e := stringValue(acc["last_token_refresh_error"]); e != "" {
			t.Errorf("旧错误未清除: %q", e)
		}
		if e := stringValue(acc["last_token_refresh_error_at"]); e != "" {
			t.Errorf("旧错误时间戳未清除: %q", e)
		}
		if st := stringValue(acc["status"]); st != StatusNormal {
			t.Errorf("status = %q，应为 %q", st, StatusNormal)
		}
	}
	if !checked {
		t.Fatal("未找到重登后的账号")
	}

	// 锚点已重置 => 不应被立刻挑进 keepalive
	if got := svc.ListRefreshTokenKeepaliveTokens(); len(got) != 0 {
		t.Errorf("重登后仍被挑入 keepalive: %v", got)
	}
}

// 回归点 8：上游的 refresh_token_reused 必须被识别为「拒绝」而非临时故障。
//
// 实测：RT 被用过几次后上游返回 401 refresh_token_reused
// （"Your refresh token has already been used to generate a new access token"）。
// 这是永久失败 —— 必须走重登，而不是无限重试。
func TestRefreshTokenReusedIsTreatedAsRejection(t *testing.T) {
	// 构造与上游实际返回一致的错误形状
	rejected := &RefreshTokenRejectedError{
		StatusCode: 401,
		Detail:     "map[code:refresh_token_reused message:Your refresh token has already been used]",
	}
	if !IsRefreshTokenRejected(rejected) {
		t.Fatal("RefreshTokenRejectedError 未被识别为拒绝")
	}
	// 包装后仍能识别（errors.As 穿透）
	wrapped := fmt.Errorf("keepalive: %w", rejected)
	if !IsRefreshTokenRejected(wrapped) {
		t.Error("包装后的错误未被识别（errors.As 未生效）")
	}
	// 其他错误不算拒绝
	for _, e := range []error{
		errors.New("oauth_refresh_http_429: rate limited"),
		errors.New("oauth_refresh_http_500: server error"),
	} {
		if IsRefreshTokenRejected(e) {
			t.Errorf("%v 被误判为拒绝", e)
		}
	}
}

// 回归：手工刷新与 watcher 不能并发处理同一个账号。
//
// 之前两条路径之间没有互斥，一次上游失效会被各记一次 invalid_count，
// 账号更快被判定失效（实测 invalid_count=2）。
func TestConcurrentRefreshDeduplicatesSameAccount(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "type": "free", "status": StatusNormal,
			"created_at": "2020-01-01 00:00:00"},
	})

	var calls atomic.Int64
	verify := func(ctx context.Context, token, event string) (Account, error) {
		calls.Add(1)
		time.Sleep(80 * time.Millisecond) // 让第二轮一定撞上在飞标记
		return Account{"access_token": token, "email": "a@x.test", "quota": 1, "status": StatusNormal}, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.RefreshAccounts(context.Background(), []string{"at-1"}, "", verify)
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("同一账号被并发刷新 %d 次，应为 1 次", got)
	}
	// 锁必须已释放：再刷一次应当能真正执行
	calls.Store(0)
	svc.RefreshAccounts(context.Background(), []string{"at-1"}, "", verify)
	if got := calls.Load(); got != 1 {
		t.Errorf("刷新权未释放：后续刷新被跳过（verify 调用 %d 次）", got)
	}
}

// 回归：已完成进度读取后应被清理，避免 sync.Map 无界增长。
func TestRefreshProgressCleanedAfterDone(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)
	svc.InitRefreshProgress("p1", 1)
	svc.FinishRefreshProgress("p1", map[string]any{"refreshed": 1}, "")

	if got := svc.GetRefreshProgress("p1"); got == nil {
		t.Fatal("第一次读取应返回完整进度")
	}
	if got := svc.GetRefreshProgress("p1"); got != nil {
		t.Error("已完成进度未被清理，sync.Map 会无界增长")
	}
}
