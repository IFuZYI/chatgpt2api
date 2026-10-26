package service

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 新账号必须拿到 account_id，且同一个输入重复归一化得到同一个值。
//
// 为什么需要稳定 ID：access_token 会被轮换（RT 换 AT、密码重登），
// 拿它当引用键时「同一个账号」在轮换前后是两个不同的键 ——
// SQL 后端甚至会把旧行删掉、新行插入，自增 id 从 1 变成 3。
func TestNormalizeAccountGeneratesStableAccountID(t *testing.T) {
	item := map[string]any{"access_token": "at-1", "email": "a@x.test"}

	first := normalizeAccount(item)
	if first == nil {
		t.Fatal("归一化返回 nil")
	}
	id, _ := first["account_id"].(string)
	if id == "" {
		t.Fatal("account_id 未生成 —— 无法稳定引用账号")
	}

	second := normalizeAccount(item)
	if got, _ := second["account_id"].(string); got != id {
		t.Errorf("重复归一化得到不同 id：%q → %q（每次重启都会变）", id, got)
	}
	t.Logf("account_id = %s", id)
}

// 已有 account_id 的记录必须原样保留，不能被重新生成。
func TestNormalizeAccountPreservesExistingAccountID(t *testing.T) {
	const fixed = "11111111-2222-3333-4444-555555555555"
	got := normalizeAccount(map[string]any{
		"access_token": "at-1", "account_id": fixed,
	})
	if id, _ := got["account_id"].(string); id != fixed {
		t.Errorf("account_id 被改写：%q，应为 %q", id, fixed)
	}
}

// 历史账号（没有 account_id）必须**确定性**派生，不能随机生成。
//
// 随机会导致每次启动给同一账号一个新 id —— 前端选中态、外部系统引用
// 全部错乱。派生种子优先 user_id，其次 email，最后才用 access_token。
func TestLegacyAccountGetsDeterministicAccountID(t *testing.T) {
	legacy := map[string]any{
		"access_token": "at-legacy",
		"email":        "legacy@x.test",
		"user_id":      "user-stable-1",
	}

	first := normalizeAccount(legacy)
	second := normalizeAccount(legacy)
	a, _ := first["account_id"].(string)
	b, _ := second["account_id"].(string)

	if a == "" {
		t.Fatal("历史账号未派生 account_id")
	}
	if a != b {
		t.Errorf("派生结果不稳定：%q vs %q", a, b)
	}

	// 同一 user_id、不同 token 必须得到同一个 id —— 这是「稳定」的含义。
	rotated := map[string]any{
		"access_token": "at-legacy-ROTATED",
		"email":        "legacy@x.test",
		"user_id":      "user-stable-1",
	}
	after := normalizeAccount(rotated)
	if c, _ := after["account_id"].(string); c != a {
		t.Errorf("token 轮换后 id 变了：%q → %q（引用会断）", a, c)
	}
}

// 只有 access_token 的历史账号也要能拿到 id（导入时还没有 email/user_id）。
func TestLegacyAccountWithoutUpstreamIDsStillGetsID(t *testing.T) {
	got := normalizeAccount(map[string]any{"access_token": "only-token"})
	if got == nil {
		t.Fatal("归一化返回 nil")
	}
	id, _ := got["account_id"].(string)
	if id == "" {
		t.Error("只有 token 的账号也应派生 account_id")
	}
	if !strings.Contains(id, "-") {
		t.Errorf("account_id 应是 UUID 形态，实际 %q", id)
	}
}

// account_id 必须与 access_token 不同 —— 否则轮换后依然不稳定。
func TestAccountIDDiffersFromAccessToken(t *testing.T) {
	got := normalizeAccount(map[string]any{"access_token": "at-abc"})
	id, _ := got["account_id"].(string)
	if id == "at-abc" {
		t.Error("account_id 不能等于 access_token")
	}
}

// 核心性质：token 轮换后 account_id 必须不变。
//
// 这是引入 account_id 的**全部意义**。若轮换会改变它，那它和
// access_token 一样不稳定，加它就没有价值。
func TestAccountIDSurvivesTokenRotation(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-old", "refresh_token": "rt-1", "email": "a@x.test",
			"user_id": "user-1", "type": "free", "status": StatusNormal},
	})
	before := svc.GetAccount("at-old")
	idBefore, _ := before["account_id"].(string)
	if idBefore == "" {
		t.Fatal("初始就没有 account_id")
	}

	refresh := func(ctx context.Context, rt string) (map[string]string, error) {
		return map[string]string{"access_token": "at-new", "refresh_token": "rt-2"}, nil
	}
	svc.KeepaliveRefreshTokens(context.Background(), []string{"at-old"}, refresh)

	after := svc.GetAccount("at-new")
	if after == nil {
		t.Fatal("轮换后找不到账号")
	}
	if got, _ := after["account_id"].(string); got != idBefore {
		t.Errorf("轮换改变了 account_id：%q → %q", idBefore, got)
	}
}

// 密码重登后 account_id 必须不变。
//
// **这条测试的能力边界（诚实说明）**：它验证「重登路径不会改变 id」，
// 但**无法**区分「id 被原样保留」与「id 被删掉后重新派生」——
// 因为种子（email / user_id）在重登前后不变，重新派生必然得到同一个值。
//
// 曾试图用「只用 access_token 做种子」来区分，但 ReloginAccounts 要求
// email 才能发起登录，而 email 一旦存在就会成为种子。所以这个区分
// 在测试层面做不到。
//
// 真正的保护在别处：
//   - 代码层面 ApplyReloginTokens 用 copyMap 保留全部字段（review 已核）
//   - 集成层面：真实号池 260 个账号经多次重登，id 全部不变（人工验证）
//
// 保留这条测试仍有价值：它至少能捕获「重登把 id 改成别的值」这类回归。
func TestAccountIDSurvivesRelogin(t *testing.T) {
	// 必须有 email + password，否则 ReloginAccounts 会以 no_email_in_account
	// 跳过 —— 测试就变成「重登失败时 id 不变」，测不到轮换路径。
	// 同时**不给 user_id/email 之外的稳定字段**？不行：email 是必需的。
	// 那就让种子落在 access_token 上（user_id 为空时 email 会先命中）——
	// 所以这里断言的是「重登路径确实换了 token」+「id 不变」两件事。
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "refresh_token": "rt-1", "email": "relogin@x.test",
			"password": "pw", "type": "free", "status": StatusAbnormal},
	})
	idBefore, _ := svc.GetAccount("at-1")["account_id"].(string)
	if idBefore == "" {
		t.Fatal("初始没有 account_id")
	}

	login := func(ctx context.Context, email, password, totp string) (map[string]string, error) {
		return map[string]string{"access_token": "at-fresh", "refresh_token": "rt-fresh"}, nil
	}
	svc.ReloginAccounts(context.Background(), []string{"at-1"}, "", login)

	accounts := svc.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("账号数 %d，应为 1", len(accounts))
	}
	// 先确认重登真的换了 token —— 否则这条测试没走到轮换路径
	if token := stringValue(accounts[0]["access_token"]); token != "at-fresh" {
		t.Fatalf("重登未生效，token 仍是 %q —— 测试没覆盖轮换路径", token)
	}
	if got, _ := accounts[0]["account_id"].(string); got != idBefore {
		t.Errorf("重登改变了 account_id：%q → %q", idBefore, got)
	}
}

// 不同账号必须拿到不同 account_id。
func TestAccountIDsAreDistinct(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "a@x.test", "user_id": "user-1", "type": "free", "status": StatusNormal},
		{"access_token": "at-2", "email": "b@x.test", "user_id": "user-2", "type": "free", "status": StatusNormal},
	})
	seen := map[string]string{}
	for _, account := range svc.ListAccounts() {
		id, _ := account["account_id"].(string)
		if id == "" {
			t.Fatal("有账号缺 account_id")
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("account_id 冲突：%s 与 %s 都是 %s", prev, account["access_token"], id)
		}
		seen[id] = stringValue(account["access_token"])
	}
}

// account_id 必须能被详情接口按 ID 查到（不只是按 token）。
func TestDetailAcceptsAccountID(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "a@x.test", "user_id": "user-1", "type": "free", "status": StatusNormal},
	})
	id, _ := svc.GetAccount("at-1")["account_id"].(string)

	byID := svc.Detail(id)
	if byID == nil {
		t.Fatal("按 account_id 查不到详情 —— 稳定标识无法用于引用")
	}
	if got := stringValue(byID["access_token"]); got != "at-1" {
		t.Errorf("按 ID 查到的是别的账号：%s", got)
	}
}

// 历史账号加载后必须被确定性回填。
//
// 三种种子都要覆盖：有 user_id、只有 email、只有 access_token。
// 随机生成会让每次重启换一批 id —— 前端选中态与外部引用全部错乱。
func TestLegacyAccountsBackfilledDeterministically(t *testing.T) {
	legacy := []map[string]any{
		{"access_token": "at-1", "email": "a@x.test", "user_id": "user-1", "type": "free", "status": StatusNormal},
		{"access_token": "at-2", "email": "b@x.test", "type": "free", "status": StatusNormal},
		{"access_token": "at-3", "type": "free", "status": StatusNormal},
	}
	first, _ := newTestAccountService(t, legacy)
	second, _ := newTestAccountService(t, legacy)

	ids := map[string]string{}
	for _, account := range first.ListAccounts() {
		token := stringValue(account["access_token"])
		id := stringValue(account["account_id"])
		if id == "" {
			t.Errorf("%s 未回填 account_id", token)
		}
		ids[token] = id
	}

	for _, account := range second.ListAccounts() {
		token := stringValue(account["access_token"])
		if got := stringValue(account["account_id"]); got != ids[token] {
			t.Errorf("%s 两次加载得到不同 id：%q vs %q", token, ids[token], got)
		}
	}
}

// account_id 必须唯一 —— 即使两个账号的派生种子相同。
//
// 回归点：派生只用 user_id/email 作为种子。若两个账号共享同一个 user_id
// 或同一个 email（重复导入、同一邮箱多账号），会得到**完全相同的**
// account_id。后果：
//   - 前端 React key 重复 → 渲染错乱、选中态互相影响
//   - findByAccountIDLocked 返回 map 迭代中的第一个匹配 ——
//     Go 的 map 顺序随机，同一个 ID 可能解析到不同账号
//   - 外部系统按 ID 引用时指向不确定的账号
func TestAccountIDsUniqueWhenSeedsCollide(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "dup@x.test", "user_id": "user-same", "type": "free", "status": StatusNormal},
		{"access_token": "at-2", "email": "dup@x.test", "user_id": "user-same", "type": "free", "status": StatusNormal},
	})

	ids := map[string]string{}
	for _, account := range svc.ListAccounts() {
		token := stringValue(account["access_token"])
		id := stringValue(account["account_id"])
		if id == "" {
			t.Fatalf("%s 没有 account_id", token)
		}
		ids[token] = id
	}

	if ids["at-1"] == ids["at-2"] {
		t.Errorf("两个账号共享 account_id %s —— 引用会指向不确定的账号", ids["at-1"])
	}
}

// 按 account_id 查询必须稳定解析到同一个账号（不能依赖 map 迭代顺序）。
func TestDetailByAccountIDIsStable(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "dup@x.test", "user_id": "user-same", "type": "free", "status": StatusNormal},
		{"access_token": "at-2", "email": "dup@x.test", "user_id": "user-same", "type": "free", "status": StatusNormal},
	})

	accounts := svc.ListAccounts()
	if len(accounts) != 2 {
		t.Fatalf("账号数 %d，应为 2", len(accounts))
	}
	for _, account := range accounts {
		id := stringValue(account["account_id"])
		want := stringValue(account["access_token"])

		// 反复查，必须每次都解析到同一个账号
		for i := 0; i < 50; i++ {
			got := svc.Detail(id)
			if got == nil {
				t.Fatalf("按 %s 查不到", id)
			}
			if token := stringValue(got["access_token"]); token != want {
				t.Fatalf("按 id=%s 解析不稳定：期望 %s，第 %d 次得到 %s", id, want, i, token)
			}
		}
	}
}

// 重复导入同一邮箱/同一 user_id 的账号时，新增路径也要消歧。
//
// 这是很现实的场景：注册机重新注册、或用户重复导入同一批账号，
// 会得到「同 email、不同 token」的两条记录。若派生不做消歧，
// 第二条会拿到与第一条相同的 account_id。
func TestAddAccountsDisambiguatesSharedSeed(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)

	svc.AddAccounts([]string{"at-first"}, "")
	svc.AddAccountItems([]map[string]any{
		{"access_token": "at-second", "email": "same@x.test", "user_id": "user-same"},
	}, nil)
	svc.AddAccountItems([]map[string]any{
		{"access_token": "at-third", "email": "same@x.test", "user_id": "user-same"},
	}, nil)

	seen := map[string]string{}
	for _, account := range svc.ListAccounts() {
		id := stringValue(account["account_id"])
		token := stringValue(account["access_token"])
		if id == "" {
			t.Errorf("%s 没有 account_id", token)
			continue
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("account_id 冲突：%s 与 %s 都是 %s", prev, token, id)
		}
		seen[id] = token
	}

	// 再导入一次也不能破坏已有分配
	before := map[string]string{}
	for _, account := range svc.ListAccounts() {
		before[stringValue(account["access_token"])] = stringValue(account["account_id"])
	}
	svc.AddAccountItems([]map[string]any{
		{"access_token": "at-fourth", "email": "same@x.test", "user_id": "user-same"},
	}, nil)
	for _, account := range svc.ListAccounts() {
		token := stringValue(account["access_token"])
		if prev, ok := before[token]; ok {
			if got := stringValue(account["account_id"]); got != prev {
				t.Errorf("%s 的 account_id 在后续导入中变了：%q → %q", token, prev, got)
			}
		}
	}
}

// 磁盘上已经重复的 account_id 必须被修复。
//
// 回归点：disambiguateAccountIDs 的第一轮遇到重复 id 时对第二个账号
// `continue`（没占位），第二轮又因 `id != "" && persisted[token] && taken[id]`
// 判定「已在第一轮占位成功」而继续跳过 —— 于是它永远不会被重新派生，
// 重复 id 留在磁盘上，按 ID 查询继续漂移。
//
// 可达性：account_id 是文档里公开的可写上传字段，用户可以传两个相同的。
func TestDisambiguateRepairsDuplicatesOnDisk(t *testing.T) {
	// 直接测 disambiguateAccountIDs —— 它是修复的入口
	accounts := map[string]Account{
		"at-1": {"access_token": "at-1", "account_id": "dup-id", "email": "a@x.test"},
		"at-2": {"access_token": "at-2", "account_id": "dup-id", "email": "b@x.test"},
	}
	persisted := map[string]bool{"at-1": true, "at-2": true}

	changed := disambiguateAccountIDs(accounts, persisted)
	if !changed {
		t.Fatal("没有报告改动 —— 重复 id 未被修复")
	}

	ids := map[string]bool{}
	for token, account := range accounts {
		id := stringValue(account["account_id"])
		if id == "" {
			t.Errorf("%s 没有 id", token)
			continue
		}
		if ids[id] {
			t.Errorf("重复 id 仍在: %s", id)
		}
		ids[id] = true
	}
	if len(ids) != 2 {
		t.Errorf("唯一 id 数 %d，应为 2", len(ids))
	}
}

// 修复后按 ID 查询必须稳定（不再漂移）。
func TestDuplicateIDsRepairedThenStable(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-a", "account_id": "same-id", "email": "a@x.test", "type": "free", "status": StatusNormal},
		{"access_token": "at-b", "account_id": "same-id", "email": "b@x.test", "type": "free", "status": StatusNormal},
	})

	// 加载后应已消歧
	seen := map[string]string{}
	for _, account := range svc.ListAccounts() {
		id := stringValue(account["account_id"])
		if prev, dup := seen[id]; dup {
			t.Errorf("加载后仍有重复 id: %s（%s 与 %s）", id, prev, stringValue(account["access_token"]))
		}
		seen[id] = stringValue(account["access_token"])
	}

	// 每个 id 反复查必须稳定
	for _, account := range svc.ListAccounts() {
		id := stringValue(account["account_id"])
		want := stringValue(account["access_token"])
		for i := 0; i < 30; i++ {
			got := svc.Detail(id)
			if got == nil {
				t.Fatalf("按 %s 查不到", id)
			}
			if token := stringValue(got["access_token"]); token != want {
				t.Fatalf("id=%s 解析漂移：期望 %s，第 %d 次得到 %s", id, want, i, token)
			}
		}
	}
}

// 已完成的进度必须能被清理，否则 sync.Map 无界增长。
//
// 回归点：GetRefreshProgress 在返回 done=true 时删除条目，但只有
// **客户端再次轮询** 才会触发。若客户端拿到结果后直接关页面、
// 或请求中途失败没走 Finish 路径，条目永久残留 —— Progress 里带
// Results 切片（每个账号一条），管理员刷 N 次就留 N 份快照。
//
// 这条测试锁住「存在一个不依赖客户端轮询的清理入口」。
func TestRefreshProgressCanBeSwept(t *testing.T) {
	svc, _ := newTestAccountService(t, []map[string]any{
		{"access_token": "at-1", "email": "sweep@x.test", "type": "free", "status": StatusNormal},
	})

	id := "sweep-test-id"
	svc.InitRefreshProgress(id, 1)
	svc.FinishRefreshProgress(id, map[string]any{"refreshed": 1}, "")

	// 模拟「客户端再也没来读」：直接调清理，应能移除
	svc.CleanRefreshProgress(id)
	if got := svc.GetRefreshProgress(id); got != nil {
		t.Errorf("清理后仍能读到进度，条目未释放")
	}
}

// 清理一个不存在的 id 不能 panic。
func TestCleanRefreshProgressUnknownIDIsSafe(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)
	svc.CleanRefreshProgress("does-not-exist")
	svc.CleanRefreshProgress("")
}

// 进度条目必须有「按时间过期」的兜底清理 —— 不能只依赖客户端轮询。
//
// 真实泄漏路径：
//  1. 客户端拿到 done=true 后关页面（GetRefreshProgress 的删除只在
//     读到 done 时才执行，之后再没人读）
//  2. 刷新请求 panic / 提前 return，没走 FinishRefreshProgress
//  3. 服务端重启前积累的条目（进程退出才释放，但长跑服务不会退）
//
// Progress 里带 Results 切片，每个账号一条 —— 261 个账号刷一次就是
// 261 条记录常驻。刷 200 次 = 5 万条。
func TestProgressEntriesExpireByAge(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)

	// 造 3 个「陈旧」条目
	for i, id := range []string{"old-1", "old-2", "old-3"} {
		svc.InitRefreshProgress(id, 1)
		svc.FinishRefreshProgress(id, map[string]any{"refreshed": i}, "")
	}

	// 把它们的时间戳改成很久以前
	svc.backdateProgressForTest("refresh", []string{"old-1", "old-2", "old-3"}, 48*time.Hour)

	// 清理超过 1 小时的条目
	removed := svc.SweepStaleProgress(1 * time.Hour)
	if removed != 3 {
		t.Errorf("清理了 %d 条，应为 3 条", removed)
	}
	for _, id := range []string{"old-1", "old-2", "old-3"} {
		if got := svc.GetRefreshProgress(id); got != nil {
			t.Errorf("%s 未被清理", id)
		}
	}
}

// 新鲜的条目不能被误删。
func TestSweepStaleProgressKeepsFresh(t *testing.T) {
	svc, _ := newTestAccountService(t, nil)
	svc.InitRefreshProgress("fresh-1", 1)
	svc.FinishRefreshProgress("fresh-1", map[string]any{"refreshed": 1}, "")

	if removed := svc.SweepStaleProgress(1 * time.Hour); removed != 0 {
		t.Errorf("误删了 %d 条新鲜条目", removed)
	}
	if got := svc.GetRefreshProgress("fresh-1"); got == nil {
		t.Error("新鲜条目被误删")
	}
}
