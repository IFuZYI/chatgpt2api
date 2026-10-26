"use client";

import { useEffect } from "react";

// 根布局级别的兜底：layout.tsx 本身抛错时 error.tsx 不会生效，
// 只有 global-error.tsx 能接住。它必须自带 <html>/<body>。
export default function GlobalError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    console.error("[chatgpt2api] 全局错误:", error);
  }, [error]);

  return (
    <html lang="zh-CN">
      <body style={{ margin: 0, fontFamily: "system-ui, sans-serif", background: "#fafaf9" }}>
        <div style={{ display: "flex", minHeight: "100vh", alignItems: "center", justifyContent: "center", padding: 24 }}>
          <div style={{ maxWidth: 520, width: "100%", background: "#fff", border: "1px solid #e7e5e4", borderRadius: 16, padding: 24 }}>
            <h2 style={{ margin: 0, fontSize: 18, color: "#292524" }}>应用出错了</h2>
            <p style={{ marginTop: 8, fontSize: 14, color: "#57534e" }}>
              你的数据没有受影响。可以重试，或刷新页面。
            </p>
            <pre style={{ marginTop: 12, padding: 12, background: "#fafaf9", borderRadius: 8, fontSize: 12, color: "#57534e", overflow: "auto", maxHeight: 160 }}>
              {error.message || "未知错误"}
            </pre>
            <button
              type="button"
              onClick={reset}
              style={{ marginTop: 16, padding: "8px 16px", borderRadius: 8, border: "none", background: "#1c1917", color: "#fff", fontSize: 14, cursor: "pointer" }}
            >
              重试
            </button>
          </div>
        </div>
      </body>
    </html>
  );
}
