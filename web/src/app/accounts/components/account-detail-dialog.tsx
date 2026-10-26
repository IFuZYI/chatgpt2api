"use client";

import { useCallback, useEffect, useState } from "react";
import { Check, Copy, Eye, EyeOff, LoaderCircle } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { buildDetailRows, maskSecret, type DetailRow } from "@/lib/account-detail";
import { fetchAccountDetail, type AccountDetail } from "@/lib/api";
import { cn } from "@/lib/utils";

type AccountDetailDialogProps = {
  /** 要查看的账号 access_token；null 表示关闭。 */
  accessToken: string | null;
  onClose: () => void;
};

const toneClass: Record<NonNullable<DetailRow["tone"]>, string> = {
  ok: "text-emerald-600",
  warn: "text-amber-600",
  danger: "text-rose-500",
  muted: "text-stone-400",
};

/** 一行：标签在左、值在右，凭据行带遮罩与复制。 */
function DetailRowView({ row }: { row: DetailRow }) {
  const [revealed, setRevealed] = useState(false);
  const [copied, setCopied] = useState(false);

  const isSecret = typeof row.secret === "string" && row.secret !== "";
  const display = isSecret ? maskSecret(row.secret as string, revealed) : row.value;

  const handleCopy = async () => {
    const value = isSecret ? (row.secret as string) : row.value;
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      // 900ms 后复位图标。用 setTimeout 而不是 toast：这里有几十行，
      // 每行都弹一个 toast 会刷屏。
      window.setTimeout(() => setCopied(false), 900);
    } catch {
      setCopied(false);
    }
  };

  return (
    <div className="flex items-start justify-between gap-4 border-b border-stone-100 py-2 last:border-b-0">
      <span className="shrink-0 pt-0.5 text-xs font-medium text-stone-500">{row.label}</span>
      <div className="flex min-w-0 items-center gap-1.5">
        <span
          className={cn(
            "min-w-0 break-all text-right font-mono text-xs",
            row.tone ? toneClass[row.tone] : "text-stone-700",
          )}
        >
          {display}
        </span>
        {isSecret ? (
          <button
            type="button"
            className="shrink-0 rounded-md p-1 text-stone-400 transition hover:bg-stone-100 hover:text-stone-700"
            onClick={() => setRevealed((prev) => !prev)}
            aria-label={revealed ? "隐藏" : "显示"}
            title={revealed ? "隐藏" : "显示"}
          >
            {revealed ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
          </button>
        ) : null}
        {row.copyable ? (
          <button
            type="button"
            className="shrink-0 rounded-md p-1 text-stone-400 transition hover:bg-stone-100 hover:text-stone-700"
            onClick={() => void handleCopy()}
            aria-label="复制"
            title="复制"
          >
            {copied ? <Check className="size-3.5 text-emerald-600" /> : <Copy className="size-3.5" />}
          </button>
        ) : null}
      </div>
    </div>
  );
}

/**
 * 账号详情弹窗。
 *
 * 只在打开时拉数据（列表接口不含凭据原文），并在关闭时清空 ——
 * 凭据不该在关掉弹窗后还留在组件状态里。
 */
export function AccountDetailDialog({ accessToken, onClose }: AccountDetailDialogProps) {
  const [detail, setDetail] = useState<AccountDetail | null>(null);
  const [error, setError] = useState("");
  const [isLoading, setIsLoading] = useState(false);

  const load = useCallback(async (token: string) => {
    setIsLoading(true);
    setError("");
    try {
      const data = await fetchAccountDetail(token);
      setDetail(data.item);
    } catch (loadError) {
      setError(loadError instanceof Error ? loadError.message : "读取账号详情失败");
    } finally {
      setIsLoading(false);
    }
  }, []);

  useEffect(() => {
    if (!accessToken) {
      setDetail(null);
      setError("");
      return;
    }
    void load(accessToken);
  }, [accessToken, load]);

  const groups = detail ? buildDetailRows(detail as unknown as Record<string, unknown>) : [];

  return (
    <Dialog open={Boolean(accessToken)} onOpenChange={(open) => (!open ? onClose() : null)}>
      <DialogContent className="max-h-[88vh] overflow-y-auto rounded-2xl p-6 sm:max-w-2xl">
        <DialogHeader className="gap-2">
          <DialogTitle>账号详情</DialogTitle>
          <DialogDescription className="text-sm leading-6">
            包含账号凭据与令牌信息，请勿外传或截图分享。
          </DialogDescription>
        </DialogHeader>

        {isLoading ? (
          <div className="flex items-center justify-center gap-2 py-10 text-sm text-stone-500">
            <LoaderCircle className="size-4 animate-spin" />
            正在读取账号详情
          </div>
        ) : null}

        {!isLoading && error ? (
          <div className="space-y-3 py-6 text-center">
            <p className="text-sm text-rose-500">{error}</p>
            <Button
              variant="outline"
              className="h-9 rounded-xl border-stone-200"
              onClick={() => (accessToken ? void load(accessToken) : undefined)}
            >
              重试
            </Button>
          </div>
        ) : null}

        {!isLoading && !error && detail
          ? groups.map((group) => (
              <section key={group.title} className="space-y-1">
                <h3 className="pt-2 text-sm font-semibold tracking-tight text-stone-800">
                  {group.title}
                </h3>
                <div className="rounded-xl bg-stone-50/70 px-3 py-1">
                  {group.rows.map((row) => (
                    <DetailRowView key={`${group.title}-${row.label}`} row={row} />
                  ))}
                </div>
              </section>
            ))
          : null}
      </DialogContent>
    </Dialog>
  );
}
