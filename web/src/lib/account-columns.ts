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
