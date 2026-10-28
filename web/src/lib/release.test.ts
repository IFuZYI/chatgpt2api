import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { parseChangelog } from "./release.ts";

const here = dirname(fileURLToPath(import.meta.url));

// 真实 CHANGELOG 必须能解析出最新版本。
//
// 回归点：解析器早期只认 `+ [类型] 内容`，而 2.1.0 起全部改成
// `+ **粗体标题。** 说明`。于是这些版本解析出 0 项、被 filter 丢掉 ——
// 应用内的「有新版本」对话框最新只显示到 2.0.1，用户被提示升级 2.2.3
// 却看不到 2.2.3 的更新内容。
test("真实 CHANGELOG 解析出最新版本且条目非空", () => {
  const content = readFileSync(join(here, "..", "..", "..", "CHANGELOG.md"), "utf8");
  const releases = parseChangelog(content);

  assert.ok(releases.length > 0, "应至少解析出一个版本");

  const newest = releases[0];
  const versionFile = readFileSync(join(here, "..", "..", "..", "VERSION"), "utf8").trim();
  assert.equal(
    newest.version,
    versionFile,
    `最新解析出的版本应与 VERSION 一致（实际 ${newest.version} vs ${versionFile}）`,
  );
  assert.ok(
    newest.items.length > 0,
    `最新版本 ${newest.version} 应解析出至少一条更新项`,
  );
});

test("两种 bullet 格式都能解析", () => {
  const content = [
    "## 9.9.9 - 2026-01-01",
    "",
    "+ [新增] 老格式条目",
    "+ **新格式标题。** 新格式说明文字",
    "",
  ].join("\n");
  const [release] = parseChangelog(content);
  assert.equal(release.version, "9.9.9");
  assert.equal(release.items.length, 2);
  assert.equal(release.items[0].type, "新增");
  assert.match(release.items[1].content, /新格式标题/);
});

test("没有任何条目的版本被丢弃", () => {
  const content = "## 1.0.0 - 2026-01-01\n\n没有 bullet 的段落\n";
  assert.deepEqual(parseChangelog(content), []);
});
