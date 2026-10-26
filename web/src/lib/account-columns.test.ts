import test from "node:test";
import assert from "node:assert/strict";

import { ACCOUNT_COLUMNS, FIRST_COLUMN_KEY, METRIC_GRID_CLASS, rowAccentClass } from "./account-columns.ts";

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
