import test from "node:test";
import assert from "node:assert/strict";

import {
  accountDetailPath,
  buildDetailRows,
  describeTokenExpiry,
  maskSecret,
  proxySourceLabel,
  accountRowKey,
  selectAllKeys,
  pruneSelection,
  defaultUpstreamModelName,
  type DetailRow,
} from "./account-detail.ts";

// AT 剩余有效期是本功能最容易被做错的一块：
// 后端用 -1 表示「未知」，用负数表示「已过期」。两者必须区分开，
// 否则运维看到「已过期」会去重登一个其实好用的号。
test("describeTokenExpiry distinguishes unknown from expired", () => {
  assert.deepEqual(describeTokenExpiry(-1, ""), {
    known: false,
    label: "未知",
    tone: "muted",
  });

  const expired = describeTokenExpiry(-3600, "2026-01-01T00:00:00Z");
  assert.equal(expired.known, true);
  assert.equal(expired.tone, "danger");
  assert.match(expired.label, /已过期/);
});

test("describeTokenExpiry formats remaining time in coarse units", () => {
  // 2 天 3 小时
  assert.equal(describeTokenExpiry(2 * 86400 + 3 * 3600, "").label, "2 天 3 小时");
  // 45 分钟
  assert.equal(describeTokenExpiry(45 * 60, "").label, "45 分钟");
  // 30 秒
  assert.equal(describeTokenExpiry(30, "").label, "30 秒");
  // 1 小时内用 warn（快到期了，需要提前刷新）
  assert.equal(describeTokenExpiry(45 * 60, "").tone, "warn");
  assert.equal(describeTokenExpiry(2 * 86400, "").tone, "ok");
});

// 凭据脱敏：默认遮住，点「显示」才给全文。
// 遮罩必须保留首尾各 4 位，让运维能核对是哪一条，又不至于泄漏。
test("maskSecret keeps a short prefix and suffix", () => {
  const masked = maskSecret("sk-abcdefghijklmnopqrstuvwxyz", false);
  assert.match(masked, /^sk-a/);
  assert.match(masked, /wxyz$/);
  assert.ok(!masked.includes("efghijklmnop"), "middle must be hidden");
  assert.equal(maskSecret("sk-abcdefghijklmnopqrstuvwxyz", true), "sk-abcdefghijklmnopqrstuvwxyz");
});

test("maskSecret handles short and empty values without throwing", () => {
  assert.equal(maskSecret("", false), "");
  // 太短的值整体遮掉：保留首尾等于直接暴露。
  assert.equal(maskSecret("abc", false), "•••");
});

// 代理来源标签：direct 时不能显示一个假地址。
test("proxySourceLabel maps backend source to human text", () => {
  assert.equal(proxySourceLabel("account"), "账号自带");
  assert.equal(proxySourceLabel("global"), "回退全局");
  assert.equal(proxySourceLabel("direct"), "直连");
  assert.equal(proxySourceLabel(""), "直连");
});

// buildDetailRows 是弹窗的主体：把后端 item 摊平成可渲染的分组行。
// 这里断言的是「哪些字段出现在哪个分组」，因为漏掉一组的直接后果
// 就是用户看不到那部分信息，而页面不会报错。
test("buildDetailRows covers credentials, tokens and runtime groups", () => {
  const item = {
    access_token: "at-value-1234567890",
    type: "Plus",
    status: "正常",
    source_type: "web",
    email: "someone@example.com",
    user_id: "user-1",
    quota: 42,
    success: 7,
    fail: 1,
    image_inflight: 0,
    created_at: "2026-10-01 10:00:00",
    last_used_at: null,
    restore_at: null,
    password: "pw-plain",
    totp_secret: "JBSWY3DPEHPK3PXP",
    refresh_token: "rt-plain",
    id_token: "id-plain",
    session_token: "sess-plain",
    proxy: "socks5h://10.0.0.9:1080",
    proxy_source: "account",
    proxy_effective: "socks5h://10.0.0.9:1080",
    access_token_expires_in: 7200,
    access_token_expires_at: "2026-10-26T12:00:00Z",
    has_password: true,
    has_totp_secret: true,
    has_refresh_token: true,
    has_id_token: true,
    has_session_token: true,
  };

  const groups = buildDetailRows(item);
  const names = groups.map((group) => group.title);

  assert.deepEqual(names, ["基本信息", "凭据", "令牌与有效期", "运行状态", "诊断"]);

  const credentialGroup = groups.find((group) => group.title === "凭据");
  assert.ok(credentialGroup, "凭据分组必须存在，否则用户看不到密码与 2FA");
  const credentialLabels = credentialGroup.rows.map((row) => row.label);
  assert.ok(credentialLabels.includes("密码"));
  assert.ok(credentialLabels.includes("2FA 密钥"));

  const tokenGroup = groups.find((group) => group.title === "令牌与有效期");
  assert.ok(tokenGroup, "令牌分组必须存在");
  const tokenLabels = tokenGroup.rows.map((row) => row.label);
  assert.ok(tokenLabels.includes("Access Token"));
  assert.ok(tokenLabels.includes("Refresh Token"));
  assert.ok(tokenLabels.includes("ID Token"));
  assert.ok(tokenLabels.includes("Session Token"));
  assert.ok(tokenLabels.includes("AT 剩余有效期"));

  const runtimeGroup = groups.find((group) => group.title === "运行状态");
  assert.ok(runtimeGroup, "运行状态分组必须存在");
  const runtimeLabels = runtimeGroup.rows.map((row) => row.label);
  assert.ok(runtimeLabels.includes("代理来源"));
  assert.ok(runtimeLabels.includes("生效代理"));
});

// 未配置的凭据必须显示成「未配置」，而不是空字符串。
// 空字符串会让用户以为字段丢了，实际是这个号本来就没有。
test("buildDetailRows marks missing credentials explicitly", () => {
  const groups = buildDetailRows({
    access_token: "at",
    type: "free",
    status: "正常",
    has_password: false,
    has_totp_secret: false,
    has_refresh_token: false,
    has_id_token: false,
    has_session_token: false,
    password: "",
    totp_secret: "",
    refresh_token: "",
    id_token: "",
    session_token: "",
    proxy_source: "direct",
    proxy_effective: "",
    access_token_expires_in: -1,
    access_token_expires_at: "",
  });

  const allRows = groups.flatMap((group) => group.rows);
  const passwordRow = allRows.find((row) => row.label === "密码");
  assert.ok(passwordRow, "密码行必须存在");
  assert.equal(passwordRow.value, "未配置");
  assert.equal(passwordRow.secret, undefined);

  const proxyRow = allRows.find((row) => row.label === "生效代理");
  assert.ok(proxyRow, "生效代理行必须存在");
  assert.equal(proxyRow.value, "直连");
});

// 详情请求的路径必须转义 token。
//
// access_token 可能是 JWT（含 "."）、也可能是 session 形态（含 "+" "/" "="）。
// 不转义时 "+" 会被后端当成空格、"&" 会截断参数 —— 表现为「账号存在但查不到」。
test("accountDetailPath percent-encodes the token", () => {
  assert.equal(
    accountDetailPath("a+b/c=d"),
    "/api/accounts/detail?access_token=a%2Bb%2Fc%3Dd",
  );
  // JWT 的 "." 不是保留字符，保持可读
  assert.equal(
    accountDetailPath("h.p.s"),
    "/api/accounts/detail?access_token=h.p.s",
  );
});

// ── 诊断字段 ──────────────────────────────────────────────
//
// 这些字段后端一直在返回（copyMap 带出来的账号原始字段），但 UI 此前
// 完全不展示。它们恰恰是「这个号为什么失效」的答案：
//   invalid_count         连续失效次数（驱动异常账号退避）
//   last_invalid_at       最后一次失效时间
//   last_refresh_error    最后一次刷新失败原因
//   last_token_refresh_*  RT 续期失败原因与时间
//   default_model_slug    上游默认模型
//
// 没有它们，运维看到「异常」只能猜；有了就能判断是 token 死了、
// 代理不通、还是上游抖动。

test("buildDetailRows exposes diagnostic fields for a failed account", () => {
  const groups = buildDetailRows({
    access_token: "eyJhbGciOi...",
    status: "异常",
    invalid_count: 4,
    last_invalid_at: "2026-10-25 20:11:00",
    last_refresh_error: "/backend-api/me failed: status=401",
    last_refresh_error_at: "2026-10-25 20:11:00",
    last_token_refresh_at: "2026-10-22 08:00:00",
    last_token_refresh_error: "oauth_refresh_http_401: refresh_token_reused",
    last_token_refresh_error_at: "2026-10-25 19:00:00",
    default_model_slug: "auto",
  });

  const flat = new Map<string, DetailRow>();
  for (const g of groups) {
    for (const r of g.rows) {
      flat.set(r.label, r);
    }
  }

  // 每个诊断字段都必须出现，且值来自后端
  const expectations: Array<[string, string]> = [
    ["连续失效次数", "4"],
    ["最近失效时间", "2026-10-25 20:11:00"],
    ["最近刷新错误", "/backend-api/me failed: status=401"],
    ["RT 最近续期", "2026-10-22 08:00:00"],
    ["RT 续期错误", "oauth_refresh_http_401: refresh_token_reused"],
    ["默认模型", "auto"],
  ];
  for (const [label, want] of expectations) {
    const row = flat.get(label);
    assert.ok(row, `缺少「${label}」这一行 —— 运维看不到诊断信息`);
    assert.equal(row.value, want, `「${label}」的值不对`);
  }
});

test("buildDetailRows shows a dash when diagnostic fields are absent", () => {
  const groups = buildDetailRows({ access_token: "at-1", status: "正常" });
  const flat = new Map<string, DetailRow>();
  for (const g of groups) {
    for (const r of g.rows) {
      flat.set(r.label, r);
    }
  }
  // 健康账号没有错误记录 —— 必须显示占位符而不是 "undefined" / "null"
  for (const label of ["最近失效时间", "最近刷新错误", "RT 续期错误"]) {
    const row = flat.get(label);
    assert.ok(row, `缺少「${label}」`);
    assert.equal(row.value, "—", `「${label}」空值应显示占位符`);
  }
});

// ── 稳定引用键 ────────────────────────────────────────────
//
// React key 与选中态此前用 access_token。token 会被轮换（RT 换 AT、密码重登），
// 轮换后 React 认为这是新行 —— 选中态、展开态全部丢失；而用户明明没动过。
//
// account_id 终身不变，是唯一合适的引用键。

test("accountRowKey prefers account_id over access_token", () => {
  assert.equal(
    accountRowKey({ account_id: "acc-1", access_token: "at-1" }),
    "acc-1",
  );
});

test("accountRowKey falls back to access_token for legacy rows", () => {
  // 老数据可能还没有 account_id（后端会在下次保存时回填）
  assert.equal(accountRowKey({ access_token: "at-legacy" }), "at-legacy");
});

test("selection survives a token rotation", () => {
  // 同一个账号，轮换前后 token 不同但 account_id 相同
  const before = { account_id: "acc-1", access_token: "at-OLD" };
  const after = { account_id: "acc-1", access_token: "at-NEW" };

  assert.equal(accountRowKey(before), accountRowKey(after));

  const selected = new Set([accountRowKey(before)]);
  assert.ok(selected.has(accountRowKey(after)), "轮换后选中态不应丢失");
});

test("buildDetailRows exposes account_id for external references", () => {
  const groups = buildDetailRows({ access_token: "at-1", account_id: "acc-9" });
  const flat = new Map<string, DetailRow>();
  for (const g of groups) {
    for (const r of g.rows) flat.set(r.label, r);
  }
  const row = flat.get("账号 ID");
  assert.ok(row, "详情里应显示 account_id，供外部系统引用");
  assert.equal(row.value, "acc-9");
  assert.equal(row.copyable, true, "account_id 应可复制");
});

// ── 选中态与全选（C2 回归） ────────────────────────────────
//
// 上一版把行 key、表头全选判定、selectedTokens 改成 accountRowKey，
// 但 toggleSelectAll 与 6 处 reload 剪枝仍按 access_token ——
// 结果：点全选勾不上、取消全选清不掉、每次刷新列表选中全丢。

test("selectAllKeys returns stable keys for every row", () => {
  const rows = [
    { account_id: "acc-1", access_token: "at-OLD-1" },
    { account_id: "acc-2", access_token: "at-OLD-2" },
  ];
  const keys = selectAllKeys(rows);
  assert.deepEqual(keys, ["acc-1", "acc-2"]);
});

test("pruneSelection keeps selections whose account still exists after rotation", () => {
  // 选中了 acc-1；列表刷新后 token 变了但 account_id 不变
  const selected = ["acc-1", "acc-2"];
  const rows = [
    { account_id: "acc-1", access_token: "at-NEW-1" },
    { account_id: "acc-3", access_token: "at-NEW-3" },
  ];
  const pruned = pruneSelection(selected, rows);
  assert.deepEqual(pruned, ["acc-1"], "acc-1 仍在列表里，必须保留；acc-2 已消失，应剪掉");
});

test("pruneSelection drops everything when the pool becomes empty", () => {
  assert.deepEqual(pruneSelection(["acc-1"], []), []);
});

test("toggleSelectAll select-all then unselect-all round-trips", () => {
  const rows = [
    { account_id: "acc-1", access_token: "at-1" },
    { account_id: "acc-2", access_token: "at-2" },
  ];
  const afterSelect = selectAllKeys(rows);
  assert.equal(afterSelect.length, 2);

  // 全选后，表头应判定为「已全选」
  assert.ok(afterSelect.every((k) => pruneSelection(afterSelect, rows).includes(k)));

  // 取消全选 = 剪掉当前页所有 key
  const afterUnselect = pruneSelection(afterSelect, []);
  assert.equal(afterUnselect.length, 0);
});

// ── 默认上游模型名（与后端对齐） ──────────────────────────
//
// 后端 DefaultUpstreamModelName() 的回退值已是 "auto"（gpt-5-5 已从上游
// 模型列表消失）。前端 store 的回退值必须一致，否则：
//   - 设置页显示 gpt-5-5，用户以为当前值就是它
//   - 保存时把 gpt-5-5 写回 config.json，覆盖掉后端的 auto
test("defaultUpstreamModelName falls back to auto", () => {
  assert.equal(defaultUpstreamModelName(undefined), "auto");
  assert.equal(defaultUpstreamModelName(""), "auto");
  assert.equal(defaultUpstreamModelName("   "), "auto");
});

test("defaultUpstreamModelName keeps an explicit value", () => {
  assert.equal(defaultUpstreamModelName("gpt-5-1"), "gpt-5-1");
  assert.equal(defaultUpstreamModelName("  auto  "), "auto");
});
