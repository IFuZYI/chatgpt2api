// 账号详情弹窗的展示逻辑。
//
// 刻意做成不依赖 React 的纯函数：项目没有前端测试框架，而这些逻辑
// （有效期换算、脱敏、分组）恰恰是最容易出错、也最需要断言的部分。
// 放在 .ts 里就能用 `node --test` 直接跑，不需要装 jsdom / testing-library。

/** 一行详情。secret 存在表示这行是凭据，渲染时要带「显示/隐藏」与「复制」。 */
export type DetailRow = {
  label: string;
  value: string;
  /** 凭据原文。有值时 UI 默认遮罩，点「显示」才展开。 */
  secret?: string;
  /** 这一行是否可复制（凭据与长 token 需要）。 */
  copyable?: boolean;
  /** 语义色调，用于给快过期/已过期的值上色。 */
  tone?: "ok" | "warn" | "danger" | "muted";
};

export type DetailGroup = {
  title: string;
  rows: DetailRow[];
};

export type TokenExpiry = {
  known: boolean;
  label: string;
  tone: "ok" | "warn" | "danger" | "muted";
};

/**
 * 把后端的 access_token_expires_in / _at 转成展示文案。
 *
 * 约定（与 internal/service/account_detail.go 一致）：
 *   -1  → 未知（该 token 不是 JWT，例如 session key 形态）
 *   <0  → 已过期
 *   >=0 → 剩余秒数
 *
 * 必须把「未知」和「已过期」分开：混在一起会让运维去重登一个其实好用的号。
 */
export function describeTokenExpiry(remainingSeconds: number, expiresAt: string): TokenExpiry {
  if (!Number.isFinite(remainingSeconds) || remainingSeconds < 0 && remainingSeconds !== -1) {
    return { known: true, label: "已过期", tone: "danger" };
  }
  if (remainingSeconds === -1) {
    return { known: false, label: "未知", tone: "muted" };
  }

  const suffix = expiresAt ? `（${expiresAt}）` : "";

  if (remainingSeconds < 60) {
    return { known: true, label: `${remainingSeconds} 秒${suffix}`, tone: "warn" };
  }
  if (remainingSeconds < 3600) {
    return { known: true, label: `${Math.floor(remainingSeconds / 60)} 分钟${suffix}`, tone: "warn" };
  }
  if (remainingSeconds < 86400) {
    const hours = Math.floor(remainingSeconds / 3600);
    const minutes = Math.floor((remainingSeconds % 3600) / 60);
    return {
      known: true,
      label: minutes > 0 ? `${hours} 小时 ${minutes} 分钟${suffix}` : `${hours} 小时${suffix}`,
      tone: remainingSeconds < 6 * 3600 ? "warn" : "ok",
    };
  }
  const days = Math.floor(remainingSeconds / 86400);
  const hours = Math.floor((remainingSeconds % 86400) / 3600);
  return {
    known: true,
    label: hours > 0 ? `${days} 天 ${hours} 小时${suffix}` : `${days} 天${suffix}`,
    tone: "ok",
  };
}

/**
 * 凭据脱敏。
 *
 * 保留首尾各 4 位：运维要能核对「这是哪一条」，又不至于把整串放在屏幕上
 * 被截屏/录屏带走。长度不足 12 的值整体遮掉 —— 保留首尾等于直接暴露。
 */
export function maskSecret(value: string, revealed: boolean): string {
  if (!value) {
    return "";
  }
  if (revealed) {
    return value;
  }
  if (value.length < 12) {
    return "•".repeat(Math.max(value.length, 3));
  }
  return `${value.slice(0, 4)}${"•".repeat(8)}${value.slice(-4)}`;
}

/**
 * 详情接口的请求路径。
 *
 * 必须 encodeURIComponent：access_token 可能是 JWT（含 "."），也可能是
 * session 形态（含 "+" "/" "="）。不转义时 "+" 会被后端解析成空格、
 * "&" 会截断参数 —— 症状是「账号明明在池子里，详情却 404」。
 */
export function accountDetailPath(accessToken: string): string {
  return `/api/accounts/detail?access_token=${encodeURIComponent(accessToken)}`;
}

/**
 * 账号行的稳定引用键。
 *
 * 用于 React key 与选中态。**不能用 access_token**：它会轮换
 * （RT 换 AT、密码重登），轮换后 React 认为这是新行 ——
 * 选中态与展开态全部丢失，而用户明明没动过。
 *
 * account_id 终身不变。老数据可能还没有它（后端会在下次保存时回填），
 * 所以保留 token 作为兜底。
 */
export function accountRowKey(account: { account_id?: unknown; access_token?: unknown }): string {
  const id = typeof account.account_id === "string" ? account.account_id.trim() : "";
  if (id !== "") {
    return id;
  }
  const token = typeof account.access_token === "string" ? account.access_token.trim() : "";
  return token;
}

export function proxySourceLabel(source: string): string {
  switch (source) {
    case "account":
      return "账号自带";
    case "global":
      return "回退全局";
    default:
      return "直连";
  }
}

function text(value: unknown, fallback = "—"): string {
  if (value === null || value === undefined) {
    return fallback;
  }
  const trimmed = String(value).trim();
  return trimmed === "" ? fallback : trimmed;
}

function secretRow(label: string, value: unknown, configured: unknown): DetailRow {
  const raw = text(value, "");
  const has = configured === true || raw !== "";
  if (!has) {
    return { label, value: "未配置", tone: "muted" };
  }
  return { label, value: raw, secret: raw, copyable: true };
}

/**
 * 把详情接口返回的 item 摊平成弹窗的分组行。
 *
 * 分组顺序与标题是 UI 契约：漏掉一组的表现是「用户看不到那部分信息」，
 * 页面不会报错，所以这里用测试锁住。
 */
export function buildDetailRows(item: Record<string, unknown>): DetailGroup[] {
  const expiresIn = typeof item.access_token_expires_in === "number" ? item.access_token_expires_in : -1;
  const expiresAt = text(item.access_token_expires_at, "");
  const expiry = describeTokenExpiry(expiresIn, expiresAt);

  const proxySource = text(item.proxy_source, "direct");
  const proxyEffective = text(item.proxy_effective, "");

  return [
    {
      title: "基本信息",
      rows: [
        {
          label: "账号 ID",
          value: text(item.account_id),
          copyable: Boolean(item.account_id),
        },
        { label: "邮箱", value: text(item.email), copyable: Boolean(item.email) },
        { label: "套餐类型", value: text(item.type) },
        { label: "来源", value: text(item.source_type) },
        { label: "状态", value: text(item.status) },
        { label: "额度", value: text(item.quota, "0") },
        { label: "User ID", value: text(item.user_id), copyable: Boolean(item.user_id) },
        { label: "创建时间", value: text(item.created_at) },
        { label: "最近使用", value: text(item.last_used_at) },
        { label: "恢复时间", value: text(item.restore_at) },
      ],
    },
    {
      title: "凭据",
      rows: [
        secretRow("密码", item.password, item.has_password),
        secretRow("2FA 密钥", item.totp_secret, item.has_totp_secret),
      ],
    },
    {
      title: "令牌与有效期",
      rows: [
        secretRow("Access Token", item.access_token, true),
        secretRow("Refresh Token", item.refresh_token, item.has_refresh_token),
        secretRow("ID Token", item.id_token, item.has_id_token),
        secretRow("Session Token", item.session_token, item.has_session_token),
        { label: "AT 剩余有效期", value: expiry.label, tone: expiry.tone },
      ],
    },
    {
      title: "运行状态",
      rows: [
        { label: "代理来源", value: proxySourceLabel(proxySource) },
        {
          label: "生效代理",
          value: proxyEffective === "" ? "直连" : proxyEffective,
          copyable: proxyEffective !== "",
        },
        { label: "成功次数", value: text(item.success, "0") },
        { label: "失败次数", value: text(item.fail, "0") },
        { label: "在途图片", value: text(item.image_inflight, "0") },
        { label: "默认模型", value: text(item.default_model_slug) },
      ],
    },
    // 诊断分组：回答「这个号为什么失效」。
    //
    // 这些字段后端一直在返回，但此前 UI 不展示 —— 运维看到「异常」只能猜。
    // 有了它们就能区分：token 真的死了（401）、代理不通（dial timeout）、
    // 还是上游抖动（5xx）。失败原因用 danger 色调，因为它是需要处理的信息。
    {
      title: "诊断",
      rows: [
        {
          label: "连续失效次数",
          value: text(item.invalid_count, "0"),
          tone: Number(item.invalid_count) > 0 ? "warn" : "muted",
        },
        { label: "最近失效时间", value: text(item.last_invalid_at) },
        {
          label: "最近刷新错误",
          value: text(item.last_refresh_error),
          tone: item.last_refresh_error ? "danger" : "muted",
          copyable: Boolean(item.last_refresh_error),
        },
        { label: "最近刷新错误时间", value: text(item.last_refresh_error_at) },
        { label: "RT 最近续期", value: text(item.last_token_refresh_at) },
        {
          label: "RT 续期错误",
          value: text(item.last_token_refresh_error),
          tone: item.last_token_refresh_error ? "danger" : "muted",
          copyable: Boolean(item.last_token_refresh_error),
        },
        { label: "RT 续期错误时间", value: text(item.last_token_refresh_error_at) },
      ],
    },
  ];
}
