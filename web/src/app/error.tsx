"use client";

import { useEffect } from "react";

// Next.js 的 error boundary。
//
// 为什么必须有：没有它时，任何渲染期异常都会让 Next 显示
// 「This page couldn't load / Reload to try again, or go back」并整页失效 ——
// 用户只能刷新，操作全丢。这个文件把它换成「局部报错 + 重试」。
//
// 关键点：错误详情要能看（否则无法排查），但不能泄露凭据 ——
// 只显示 message，不 dump 整个 state。
export default function ErrorBoundary({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    // 送到控制台，便于用户复制给我们排查
    console.error("[chatgpt2api] 页面渲染出错:", error);
  }, [error]);

  return (
    <div className="flex min-h-[60vh] items-center justify-center px-6">
      <div className="w-full max-w-lg rounded-2xl border border-stone-200 bg-white p-6 shadow-sm">
        <h2 className="text-lg font-semibold tracking-tight text-stone-800">
          页面出错了
        </h2>
        <p className="mt-2 text-sm text-stone-600">
          这一步没能完成，你的数据没有受影响。可以先重试；若反复出现，
          请把下面的信息反馈给我们。
        </p>
        <pre className="mt-3 max-h-40 overflow-auto rounded-lg bg-stone-50 p-3 text-xs text-stone-600">
          {error.message || "未知错误"}
          {error.digest ? `\n(digest: ${error.digest})` : ""}
        </pre>
        <div className="mt-4 flex gap-2">
          <button
            type="button"
            onClick={reset}
            className="rounded-lg bg-stone-900 px-4 py-2 text-sm font-medium text-white transition hover:bg-stone-700"
          >
            重试
          </button>
          <button
            type="button"
            onClick={() => window.location.assign("/accounts")}
            className="rounded-lg border border-stone-200 px-4 py-2 text-sm font-medium text-stone-700 transition hover:bg-stone-50"
          >
            回到号池
          </button>
        </div>
      </div>
    </div>
  );
}
