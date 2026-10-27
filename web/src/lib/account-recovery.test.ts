import test from "node:test";
import assert from "node:assert/strict";

import { RECOVERABLE_STATUSES, isRecoverableStatus, selectRecoverableTokens } from "./account-recovery.ts";

// 「尝试恢复异常账号」必须同时接受「异常」与「禁用」。
//
// 策略变更（2026-10-27）：不可恢复的账号（封号、缺 2FA 密钥、缺凭据…）
// 一律转「禁用」。如果恢复入口只认「异常」，这些账号就再也点不了恢复 ——
// 面板会变成一个只进不出的死状态，而补上 2FA 密钥/密码后本可以救回来。
test("disabled accounts are recoverable", () => {
  assert.ok(isRecoverableStatus("禁用"));
  assert.ok(isRecoverableStatus("异常"));
  assert.ok(RECOVERABLE_STATUSES.includes("禁用"));
});

// 正常/限流的账号不该出现在恢复流程里（它们不需要恢复）。
test("healthy accounts are not recoverable", () => {
  assert.equal(isRecoverableStatus("正常"), false);
  assert.equal(isRecoverableStatus("限流"), false);
});

// 未知状态不参与恢复（避免误操作）。
test("unknown status is not recoverable", () => {
  assert.equal(isRecoverableStatus(""), false);
  assert.equal(isRecoverableStatus("banned"), false);
});

// selectRecoverableTokens 过滤掉非可恢复账号，并保持输入顺序。
test("selectRecoverableTokens filters and preserves order", () => {
  const accounts = [
    { access_token: "a", status: "正常" },
    { access_token: "b", status: "异常" },
    { access_token: "c", status: "禁用" },
    { access_token: "d", status: "限流" },
  ];
  assert.deepEqual(selectRecoverableTokens(accounts, ["d", "c", "a", "b"]), ["c", "b"]);
});

// 找不到对应记录（例如已被删除）的 token 必须被丢弃，不能原样传给后端。
test("selectRecoverableTokens drops unknown tokens", () => {
  const accounts = [{ access_token: "a", status: "异常" }];
  assert.deepEqual(selectRecoverableTokens(accounts, ["ghost", "a"]), ["a"]);
});
