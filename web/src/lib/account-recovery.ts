import type { AccountStatus } from "./api.ts";

// RECOVERABLE_STATUSES 是「尝试恢复」入口接受的状态。
//
// 为什么包含「禁用」：不可恢复的账号（封号、缺 2FA 密钥、缺凭据…）
// 会被自动路径转成「禁用」终态。人工补齐凭据后，恢复入口是唯一的
// 解禁通道 —— 只认「异常」会让这些账号永远出不来。
//
// 为什么不包含「正常」「限流」：它们不需要恢复。把正常号放进恢复流程
// 会平白发起一次密码登录，白白制造风控信号。
export const RECOVERABLE_STATUSES: readonly AccountStatus[] = ["异常", "禁用"] as const;

// isRecoverableStatus 判断单个状态是否参与恢复流程。
// 未知状态一律不参与，避免对拼写错误或外部导入的状态做误操作。
export function isRecoverableStatus(status: unknown): boolean {
  if (typeof status !== "string") {
    return false;
  }
  return (RECOVERABLE_STATUSES as readonly string[]).includes(status);
}

// selectRecoverableTokens 从选中 token 里挑出可恢复的账号，保持输入顺序。
//
// 找不到对应记录的 token 会被丢弃（可能已被删除），不能原样传给后端 ——
// 否则后端会为每个不存在的 token 记一条「账号不存在」错误，把真正的
// 失败原因淹没在噪声里。
export function selectRecoverableTokens(
  accounts: ReadonlyArray<{ access_token?: string; status?: string }>,
  tokens: readonly string[],
): string[] {
  const byToken = new Map<string, { access_token?: string; status?: string }>();
  for (const account of accounts) {
    if (account.access_token) {
      byToken.set(account.access_token, account);
    }
  }
  const out: string[] = [];
  for (const token of tokens) {
    const account = byToken.get(token);
    if (account && isRecoverableStatus(account.status)) {
      out.push(token);
    }
  }
  return out;
}
