// 账号表格的列定义。
//
// 抽成纯函数是为了能用 node --test 断言列顺序与内容 ——
// 「哪一列排第一」「哪些列不该出现」这类需求，靠肉眼看 JSX 容易漏。

export type AccountColumn = {
  /** 稳定标识，用于测试与 key */
  key: string;
  /** 表头文案 */
  label: string;
  /** 列宽（Tailwind class 片段） */
  width: string;
};

/** 列定义。顺序即渲染顺序。 */
export const ACCOUNT_COLUMNS: AccountColumn[] = [
  { key: "info", label: "账号信息", width: "w-64" },
  { key: "type", label: "类型", width: "w-28" },
  { key: "status", label: "状态", width: "w-24" },
  { key: "quota", label: "额度", width: "w-24" },
  { key: "restore", label: "恢复时间", width: "w-40" },
  { key: "inflight", label: "在途", width: "w-18" },
  { key: "success", label: "成功", width: "w-18" },
  { key: "fail", label: "失败", width: "w-18" },
];

/** 第一列的 key。账号信息必须在最左。 */
export const FIRST_COLUMN_KEY = "info";

/** 行左侧状态色条：让「异常 / 限流」在一屏里一眼可辨。 */
export function rowAccentClass(status: string): string {
  switch (status) {
    case "正常":
      return "border-l-emerald-400";
    case "限流":
      return "border-l-amber-400";
    case "异常":
      return "border-l-rose-400";
    case "禁用":
      return "border-l-stone-300";
    default:
      return "border-l-transparent";
  }
}

/**
 * 统计卡片的响应式网格。
 *
 * 移动端必须 2 列而不是 1 列：6 张卡片单列堆叠会占满整个视口，
 * 用户要滚很久才看到账号列表 —— 而列表才是这页的主体。
 *
 * 2 列 × 3 行在 390px 宽下每张约 170px，仍能放下「标签 + 数字」。
 */
export const METRIC_GRID_CLASS = "grid grid-cols-2 gap-2.5 sm:gap-3 md:grid-cols-3 xl:grid-cols-6";

/**
 * 账号列表每页条数。
 *
 * 为什么要可配置且默认较小：261 个账号若一次渲染全部，每次筛选/翻页
 * 都要重建上千个 DOM 节点 —— 实测翻页 291ms、击键 271ms，明显卡顿。
 *
 * 默认 20 条：桌面一屏约 10-12 行，20 条只比一屏多一点；
 * 同时把单次渲染的节点数压在 ~300 以内。
 */
export const PAGE_SIZE_OPTIONS = [10, 20, 50, 100] as const;
export const DEFAULT_PAGE_SIZE = 20;

/**
 * 判断两行是否渲染等价 —— 供 React.memo 的比对函数使用。
 *
 * 为什么需要：261 个账号时，任何 state 变化（包括搜索框击键）都会
 * 重渲染整个表格。虽然只显示 10 行，但 React 仍要为每行重建元素树，
 * 实测击键 271ms、翻页 291ms。
 *
 * 比对只关心**实际渲染用到的字段**。刻意排除 access_token：
 * 它只在复制按钮里用，而按钮回调通过闭包拿到，不影响视觉输出；
 * 且它 2000+ 字符，逐字比较本身就有成本。
 */
export function sameAccountRow(
  a: { account_id?: unknown; email?: unknown; status?: unknown; type?: unknown; quota?: unknown; restore_at?: unknown; success?: unknown; fail?: unknown },
  b: { account_id?: unknown; email?: unknown; status?: unknown; type?: unknown; quota?: unknown; restore_at?: unknown; success?: unknown; fail?: unknown },
): boolean {
  return (
    a.account_id === b.account_id &&
    a.email === b.email &&
    a.status === b.status &&
    a.type === b.type &&
    a.quota === b.quota &&
    a.restore_at === b.restore_at &&
    a.success === b.success &&
    a.fail === b.fail
  );
}

/**
 * 搜索输入的延迟值计算 —— 纯函数，便于测试。
 *
 * 为什么需要延迟：261 个账号时，每次击键都重算 filteredAccounts 并重建
 * 整个 tbody 的元素树。实测击键 75-271ms，翻页 16-291ms，
 * 用户能感觉到输入卡顿。
 *
 * 延迟 150ms：打字速度快时（每秒 8 字以上）只在停顿时算一次；
 * 打字慢时每次都能及时响应。低于 100ms 会让快速打字仍逐字重算，
 * 高于 250ms 会让用户觉得「输入没反应」。
 */
export const SEARCH_DEBOUNCE_MS = 150;

/**
 * 判断一个查询值是否值得触发重新过滤。
 *
 * 空串与纯空白等价 —— 避免用户按空格时白跑一次全量过滤。
 */
export function normalizeQuery(raw: string): string {
  return raw.trim().toLowerCase();
}

/** 两次查询是否产生相同的过滤结果（用于跳过无意义的重算）。 */
export function sameQuery(a: string, b: string): boolean {
  return normalizeQuery(a) === normalizeQuery(b);
}

/**
 * 移动端是否默认折叠「系统可用模型」区块。
 *
 * dogfood 实测（390×844）：模型标签占满整整一屏（15 个标签换行 5 行），
 * 加上 6 张统计卡片，账号列表要滚 3 屏才出现 —— 而列表才是这页主体。
 *
 * 模型列表是「偶尔查一下」的信息，不该占据首屏。
 */
export function shouldCollapseModels(isMobile: boolean): boolean {
  return isMobile;
}

/** 移动端断点（与 Tailwind 的 sm 对齐）。 */
export const MOBILE_BREAKPOINT_PX = 640;

/**
 * 取状态元数据的兜底版本。
 *
 * 为什么必须有：`statusMeta[account.status]` 只覆盖 4 个已知状态。
 * 上游返回任何其他值（带空格、空串、新增状态、未知枚举）时索引是
 * undefined，紧接着读 `.icon` 就抛 TypeError —— React 整页崩溃，
 * 用户看到「This page couldn't load」。刷新时数据来自上游，最容易触发。
 *
 * 兜底返回「禁用」样式（灰色 + Ban 图标），并把未知值原样展示，
 * 让人能看出是哪个状态没识别，而不是白屏。
 */
export function resolveStatusMeta<T>(meta: Record<string, T>, status: unknown, fallback: T): T {
  if (typeof status === "string") {
    const trimmed = status.trim();
    if (trimmed !== "" && Object.prototype.hasOwnProperty.call(meta, trimmed)) {
      return meta[trimmed];
    }
  }
  return fallback;
}

/** 把任意状态值规整成可展示的字符串。 */
export function normalizeStatusLabel(status: unknown): string {
  if (typeof status === "string" && status.trim() !== "") {
    return status.trim();
  }
  return "未知";
}

/**
 * 构造 image_storage 的保存 payload。
 *
 * 核心规则：密码框留空时**必须带上 has_webdav_password 标记**。
 *
 * 为什么：GET /api/settings 返回的是脱敏配置（webdav_password 被换成
 * has_webdav_password）。用户不动密码框时前端拿到的是空串 —— 若直接
 * 回传空串，后端会认为「用户要清空密码」，校验失败并报
 * 「启用 WebDAV 图片存储后必须填写 WebDAV 密码」。
 *
 * 填了新密码就正常发新值，且不带 has_*（避免后端误以为「不修改」）。
 */
export function buildWebdavPayload(stored: {
  enabled?: unknown;
  mode?: unknown;
  webdav_url?: unknown;
  webdav_username?: unknown;
  webdav_password?: unknown;
  has_webdav_password?: unknown;
  webdav_root_path?: unknown;
  public_base_url?: unknown;
}): Record<string, unknown> {
  const password = String(stored.webdav_password ?? "").trim();
  const payload: Record<string, unknown> = {
    enabled: Boolean(stored.enabled),
    mode: String(stored.mode ?? "local"),
    webdav_url: String(stored.webdav_url ?? "").trim(),
    webdav_username: String(stored.webdav_username ?? "").trim(),
    webdav_password: password,
    webdav_root_path: String(stored.webdav_root_path ?? "chatgpt2api/images").trim(),
    public_base_url: String(stored.public_base_url ?? "").trim(),
  };
  // 只有「留空」时才需要 has_* 来告诉后端保留旧值。
  if (password === "") {
    payload.has_webdav_password = Boolean(stored.has_webdav_password);
  }
  return payload;
}
