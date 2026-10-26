import test from "node:test";
import assert from "node:assert/strict";

import { ACCOUNT_COLUMNS, DEFAULT_PAGE_SIZE, FIRST_COLUMN_KEY, METRIC_GRID_CLASS, PAGE_SIZE_OPTIONS, rowAccentClass, sameAccountRow, resolveStatusMeta, normalizeStatusLabel, buildWebdavPayload, withSecretFlags, SEARCH_DEBOUNCE_MS, normalizeQuery, sameQuery, shouldCollapseModels, MOBILE_BREAKPOINT_PX } from "./account-columns.ts";

// 账号首页：账号信息放第一列。
//
// 之前是 token 在第一列、账号信息在第 6 列 —— 用户要扫一眼看到「这是哪个号」，
// 却先看到一长串 JWT。
test("account info is the first column", () => {
  assert.equal(ACCOUNT_COLUMNS[0].key, FIRST_COLUMN_KEY);
  assert.equal(ACCOUNT_COLUMNS[0].label, "账号信息");
});

// token / 来源 / 创建时间 不该出现在列表里 —— 它们属于详情弹窗。
//
// 理由：token 是 2000+ 字符的 JWT，占宽且无法快速识别；
// 来源与创建时间是低频信息。列表只保留「扫一眼要用的」。
test("list does not render token, source or created_at", () => {
  const keys = ACCOUNT_COLUMNS.map((c) => c.key);
  for (const banned of ["token", "source", "created"]) {
    assert.ok(!keys.includes(banned), `列表不应有 ${banned} 列（应只在详情里）`);
  }

  const labels = ACCOUNT_COLUMNS.map((c) => c.label).join(" ");
  for (const banned of ["token", "来源", "创建时间"]) {
    assert.ok(!labels.includes(banned), `列表表头不应出现「${banned}」`);
  }
});

// 列 key 必须唯一，否则 React key 冲突。
test("column keys are unique", () => {
  const keys = ACCOUNT_COLUMNS.map((c) => c.key);
  assert.equal(new Set(keys).size, keys.length);
});

// 每列都要有宽度，否则表格会抖动。
test("every column declares a width", () => {
  for (const col of ACCOUNT_COLUMNS) {
    assert.ok(col.width && col.width.length > 0, `${col.key} 缺 width`);
  }
});

// 行左侧色条：状态必须能在一屏里被眼睛直接分组。
test("rowAccentClass gives each status a distinct colour", () => {
  const classes = ["正常", "限流", "异常", "禁用"].map(rowAccentClass);
  assert.equal(new Set(classes).size, 4, "四种状态应有四种不同色条");
  for (const cls of classes) {
    assert.ok(cls.startsWith("border-l-"), `${cls} 应是左边框色`);
  }
});

test("rowAccentClass falls back to transparent for unknown status", () => {
  assert.equal(rowAccentClass("bogus"), "border-l-transparent");
});

// 移动端统计卡片必须多列，否则占满整屏。
//
// 实测（390×844 视口）：6 张卡片单列堆叠，账号列表要滚动 3 屏才出现。
test("metric grid uses multiple columns on mobile", () => {
  assert.ok(METRIC_GRID_CLASS.includes("grid-cols-2"), "移动端应 2 列，避免占满整屏");
  assert.ok(!/^grid grid-cols-1 /.test(METRIC_GRID_CLASS), "不应是单列");
  assert.ok(METRIC_GRID_CLASS.includes("xl:grid-cols-6"), "宽屏应展示全部 6 张");
});

// 分页默认值：过大导致每次渲染重建上千 DOM 节点（实测卡顿）。
test("default page size keeps render cost bounded", () => {
  assert.ok(DEFAULT_PAGE_SIZE >= 10, "太小会让翻页过于频繁");
  assert.ok(DEFAULT_PAGE_SIZE <= 50, "超过 50 条单次渲染节点过多，会卡顿");
  assert.ok(PAGE_SIZE_OPTIONS.includes(DEFAULT_PAGE_SIZE), "默认值必须在可选项里");
});

test("page size options are ascending and unique", () => {
  const sorted = [...PAGE_SIZE_OPTIONS].sort((a, b) => a - b);
  assert.deepEqual([...PAGE_SIZE_OPTIONS], sorted, "选项应升序");
  assert.equal(new Set(PAGE_SIZE_OPTIONS).size, PAGE_SIZE_OPTIONS.length, "选项不应重复");
});

// 行比对：只有影响视觉的字段变化才重渲染。
test("sameAccountRow ignores access_token rotation", () => {
  const before = { account_id: "a", email: "x@y.z", status: "正常", type: "free", quota: 1, restore_at: null, success: 0, fail: 0, access_token: "OLD" };
  const after = { ...before, access_token: "NEW" };
  assert.equal(sameAccountRow(before, after), true, "token 轮换不该触发重渲染");
});

test("sameAccountRow detects visible field changes", () => {
  const base = { account_id: "a", email: "x@y.z", status: "正常", type: "free", quota: 1, restore_at: null, success: 0, fail: 0 };
  for (const [field, value] of [["status", "异常"], ["quota", 0], ["email", "z@y.z"], ["type", "Plus"]] as const) {
    assert.equal(sameAccountRow(base, { ...base, [field]: value }), false, `${field} 变化必须重渲染`);
  }
});

// 搜索延迟：必须在一个「打字不卡」与「输入有反馈」之间的区间。
test("search debounce is in a sane range", () => {
  assert.ok(SEARCH_DEBOUNCE_MS >= 100, "低于 100ms 快速打字仍逐字重算");
  assert.ok(SEARCH_DEBOUNCE_MS <= 250, "高于 250ms 用户会觉得输入没反应");
});

// 纯空白与空串等价，避免按空格时白跑一次全量过滤。
test("normalizeQuery treats whitespace as empty", () => {
  assert.equal(normalizeQuery(""), "");
  assert.equal(normalizeQuery("   "), "");
  assert.equal(normalizeQuery("  Laxest "), "laxest");
});

test("sameQuery ignores case and surrounding spaces", () => {
  assert.equal(sameQuery("Laxest", "laxest"), true);
  assert.equal(sameQuery("  laxest  ", "laxest"), true);
  assert.equal(sameQuery("laxest", "laxest1"), false);
  assert.equal(sameQuery("", "   "), true);
});

// 移动端必须默认折叠模型列表，否则账号列表要滚 3 屏才出现。
test("models block collapses on mobile by default", () => {
  assert.equal(shouldCollapseModels(true), true, "移动端应折叠");
  assert.equal(shouldCollapseModels(false), false, "桌面端应展开");
});

test("mobile breakpoint matches tailwind sm", () => {
  assert.equal(MOBILE_BREAKPOINT_PX, 640);
});

// 未知状态必须兜底，不能返回 undefined（否则 .icon 抛错整页崩溃）。
//
// 实测触发路径：刷新后 accounts 来自上游，status 可能是带空格、
// 空串或未枚举的新值。原实现直接 statusMeta[status] 索引。
test("resolveStatusMeta falls back for unknown status", () => {
  const meta = { 正常: { icon: "ok" }, 异常: { icon: "bad" } };
  const fallback = { icon: "unknown" };

  assert.deepEqual(resolveStatusMeta(meta, "正常", fallback), { icon: "ok" });
  assert.deepEqual(resolveStatusMeta(meta, "  异常  ", fallback), { icon: "bad" }, "应 trim");
  assert.deepEqual(resolveStatusMeta(meta, "已过期", fallback), fallback, "未知状态用兜底");
  assert.deepEqual(resolveStatusMeta(meta, "", fallback), fallback, "空串用兜底");
  assert.deepEqual(resolveStatusMeta(meta, undefined, fallback), fallback);
  assert.deepEqual(resolveStatusMeta(meta, null, fallback), fallback);
  assert.deepEqual(resolveStatusMeta(meta, 123, fallback), fallback);
});

// 原型链上的键不能被当成有效状态（防 __proto__ 之类的意外命中）。
test("resolveStatusMeta ignores inherited keys", () => {
  const meta = { 正常: { icon: "ok" } };
  const fallback = { icon: "unknown" };
  assert.deepEqual(resolveStatusMeta(meta, "toString", fallback), fallback);
  assert.deepEqual(resolveStatusMeta(meta, "__proto__", fallback), fallback);
});

test("normalizeStatusLabel handles empty and non-string", () => {
  assert.equal(normalizeStatusLabel("正常"), "正常");
  assert.equal(normalizeStatusLabel("  异常 "), "异常");
  assert.equal(normalizeStatusLabel(""), "未知");
  assert.equal(normalizeStatusLabel(null), "未知");
  assert.equal(normalizeStatusLabel(42), "未知");
});

// ── WebDAV 密码「留空 = 不修改」 ──────────────────────────
//
// 后端 GET /api/settings 返回脱敏配置（webdav_password 换成
// has_webdav_password）。前端回传时必须带上这个标记，否则后端认为
// 「用户要清空密码」→ 校验失败 → 报「必须填写 WebDAV 密码」。
test("buildWebdavPayload carries has_webdav_password when password left blank", () => {
  const stored = {
    enabled: true,
    mode: "webdav",
    webdav_url: "https://dav.example.com",
    webdav_username: "u",
    webdav_password: "",
    has_webdav_password: true,
    webdav_root_path: "chatgpt2api/images",
    public_base_url: "",
  };
  const payload = buildWebdavPayload(stored);
  assert.equal(payload.has_webdav_password, true, "必须告诉后端「已有密码，别覆盖」");
  assert.equal(payload.webdav_password, "");
});

test("buildWebdavPayload omits the flag when user types a new password", () => {
  const stored = {
    enabled: true,
    mode: "webdav",
    webdav_url: "https://dav.example.com",
    webdav_username: "u",
    webdav_password: "NEW-PASS",
    has_webdav_password: true,
  };
  const payload = buildWebdavPayload(stored);
  assert.equal(payload.webdav_password, "NEW-PASS");
  assert.equal(payload.has_webdav_password, undefined, "填了新密码就不该再发 has_*");
});

test("buildWebdavPayload keeps the flag false when there was never a password", () => {
  const payload = buildWebdavPayload({
    enabled: true,
    mode: "webdav",
    webdav_url: "https://dav.example.com",
    webdav_password: "",
    has_webdav_password: false,
  });
  assert.equal(payload.has_webdav_password, false);
});

// 用户想清空密码：has_ 为 false 且密码框留空，但 WebDAV 仍启用。
// 后端会拒绝（WebDAV 必须有密码）—— 前端不该静默失败，
// 而应把 has_ 保持为 true，让用户明白「密码还在，只是没显示」。
test("clearing password while webdav enabled keeps the has flag true", () => {
  const payload = buildWebdavPayload({
    enabled: true,
    mode: "webdav",
    webdav_url: "https://dav.example.com",
    webdav_password: "",
    has_webdav_password: true,
  });
  // 留空时必须带 true —— 否则后端会把「留空」当成「要清空」并报错
  assert.equal(payload.has_webdav_password, true);
});

// R2 密钥与 AI key 留空时必须带 has_* —— 否则后端会静默抹掉。
//
// 这比 WebDAV 密码那条更隐蔽：没有校验错误，只是某天备份连不上。
test("withSecretFlags marks blank backup secrets as unchanged", () => {
  const stored = { has_secret_access_key: true, has_passphrase: true };
  const payload = withSecretFlags(
    { secret_access_key: "", passphrase: "" },
    stored,
    ["secret_access_key", "passphrase"],
  );
  assert.equal(payload.has_secret_access_key, true);
  assert.equal(payload.has_passphrase, true);
});

test("withSecretFlags omits flags when a new value is typed", () => {
  const payload = withSecretFlags(
    { api_key: "sk-NEW" },
    { has_api_key: true },
    ["api_key"],
  );
  assert.equal(payload.api_key, "sk-NEW");
  assert.equal(payload.has_api_key, undefined);
});

test("withSecretFlags omits flags when nothing was ever stored", () => {
  const payload = withSecretFlags({ api_key: "" }, {}, ["api_key"]);
  assert.equal(payload.has_api_key, undefined);
});
