export type ReleaseInfo = {
  version: string;
  date: string;
  items: { type: string; content: string }[];
};

export function parseChangelog(content: string): ReleaseInfo[] {
  return content
    .split(/^## /m)
    .slice(1)
    .map((block) => {
      const [title = "", ...lines] = block.trim().split("\n");
      const [, version = title.trim(), date = ""] =
        title.match(/^(.+?)(?:\s+-\s+(.+))?$/) || [];
      return {
        version: version.trim(),
        date: date.trim(),
        items: lines.flatMap((line) => {
          const trimmed = line.trim();
          // 格式一（1.x 与 2.0.x）：+ [类型] 内容
          const typed = trimmed.match(/^\+\s+\[(.+?)\]\s+(.+)$/);
          if (typed) return [{ type: typed[1], content: typed[2] }];
          // 格式二（2.1.0 起）：+ **粗体标题。** 说明文字
          //
          // 早期只认格式一，于是 2.1.0 之后的所有版本都解析出 0 项、被下面的
          // filter 丢掉 —— 应用内的「有新版本」对话框最新只显示到 2.0.1，
          // 用户被提示升级 2.2.3 却看不到 2.2.3 的更新内容。
          const bold = trimmed.match(/^\+\s+\*\*(.+?)\*\*\s*(.*)$/);
          if (bold) return [{ type: "更新", content: bold[2] ? `${bold[1]} ${bold[2]}` : bold[1] }];
          return [];
        }),
      };
    })
    .filter((release) => release.items.length);
}
