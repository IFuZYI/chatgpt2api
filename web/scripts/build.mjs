// 前端构建入口：注入 app 版本号后调用 next build。
//
// 为什么需要它：`NEXT_PUBLIC_APP_VERSION` 只在构建期生效。Dockerfile 里
// 显式注入，但按 AGENTS.md 的本地流程直接跑 `npm run build` 会漏掉它 ——
// 界面上版本号显示成 0.0.0（实测：本地构建产物里搜得到 0.0.0）。
//
// 已有注入值时尊重它（Dockerfile 传了就用它的），否则读仓库根的 VERSION。
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const versionFile = join(here, "..", "..", "VERSION");

let version = (process.env.NEXT_PUBLIC_APP_VERSION ?? "").trim();
if (!version && existsSync(versionFile)) {
  version = readFileSync(versionFile, "utf8").trim();
}
if (!version) {
  console.warn("[build] 未找到 VERSION 文件，appVersion 将回退为 0.0.0");
}

execFileSync("next", ["build"], {
  stdio: "inherit",
  env: { ...process.env, NEXT_PUBLIC_APP_VERSION: version },
});
