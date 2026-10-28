#!/usr/bin/env bash
# 本地构建发布产物 —— 与 .github/workflows/release.yml 的 build job 等价。
#
# 为什么要这个脚本：Actions 被暂停时只能本地构建产物再手动建 Release，
# 而手敲那串「6 个平台 × 交叉编译 + ldflags + sha256sum」很容易漏一个平台、
# 漏 -X 版本注入，或改完代码忘记重建（实测踩过：产物比 tag 落后两个修复）。
# 把流程固化成一个脚本，并在 CI 里用测试保证它与 release.yml 不漂移。
#
# 用法：
#   scripts/build-release.sh [输出目录]        默认 ./dist-release
#   scripts/build-release.sh --allow-dirty [目录]  允许在不干净的工作区上构建
#
# 产物命名与 release.yml 完全一致，可直接上传到 GitHub Release。
#
# 脚本会**自己构建前端**再编译后端（与 release.yml 的 Build frontend +
# Stage frontend 两步对应）。这一步不能省：前端是 //go:embed all:web_dist
# 编进二进制的，而 internal/web/web_dist/ 在仓库里只有 .gitkeep 占位
# （其余被 gitignore）—— 全新克隆上直接 go build 会得到一个**没有前端、
# 访问 / 与 /accounts 全 404** 的二进制。脚本因此先构建前端，并在编译前
# 断言 index.html 真的存在。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

version="$(tr -d '[:space:]' < VERSION)"
if [ -z "$version" ]; then
  echo "VERSION 文件为空" >&2
  exit 1
fi

out_dir="${1:-dist-release}"

# 拒绝在脏工作区上构建发布产物。
#
# Go 默认把版本控制信息编进二进制（`vcs.modified` / `vcs.revision`，模块版本
# 带 `+dirty` 后缀）。加了 `-trimpath` 也照样编 —— 实测：工作区里多一个未提交
# 文件，产出的二进制就从 `vcs.modified=false` 变成 `true`、模块版本变成
# `v0.0.0-<时间>-<commit>+dirty`，校验和随之改变。
#
# 后果有两层：
#   1. 发布出去的二进制自称「dirty」，用户看到会怀疑它不对应任何提交；
#   2. 同样源码两次构建得到不同字节，无法复现。
#
# 所以这里直接拒绝，而不是静默产出一个带 +dirty 的包。确实要本地试构建时
# 传 --allow-dirty。
# 解析参数：--allow-dirty 可以出现在任意位置（早期只认第一个参数，
# `./script out --allow-dirty` 会被当成「没加 flag」而拒绝，提示还很费解）。
allow_dirty=false
positional=()
for arg in "$@"; do
  case "$arg" in
    --allow-dirty) allow_dirty=true ;;
    -h|--help)
      sed -n '1,30p' "${BASH_SOURCE[0]}" | sed 's/^# \?//'
      exit 0
      ;;
    *) positional+=("$arg") ;;
  esac
done
out_dir="${positional[0]:-dist-release}"
if [ "$allow_dirty" != "true" ]; then
  if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
    cat >&2 <<'MSG'
工作区不干净，拒绝构建发布产物 —— 否则二进制会被标记为 vcs.modified=true
且模块版本带 +dirty，与任何提交都不对应。

先 commit 或 stash，再重跑。确实只想本地试构建时加 --allow-dirty。
MSG
    git status --short >&2
    exit 1
  fi
fi

# ── 前端：构建 + 暂存进 embed 目录 ────────────────────────────────
#
# 顺序与 release.yml 一致：先 frontend，再 stage，最后 go build。
# 少了这步会产出没有前端的二进制（实测：全新克隆直接跑本脚本，
# / 与 /accounts 都返回 404）。
echo "构建前端（注入 VERSION=${version}）"
if [ ! -d web/node_modules ]; then
  ( cd web && npm ci --no-audit --no-fund )
fi
( cd web && npm run build )

# 保留 .gitkeep：它是**被跟踪**的占位文件（空目录无法 embed），
# 直接 rm -rf 会让工作区变脏 —— 于是脚本第二次运行会被自己的
# 「脏工作区」检查拦下，而且产出的字节与标准流程不同
#（.gitkeep 参与 embed，实测会改变二进制校验和）。
keep_gitkeep=""
if [ -f internal/web/web_dist/.gitkeep ]; then
  keep_gitkeep="$(mktemp)"
  cp internal/web/web_dist/.gitkeep "$keep_gitkeep"
fi

rm -rf internal/web/web_dist
mkdir -p internal/web/web_dist
cp -r web/out/. internal/web/web_dist/

if [ -n "$keep_gitkeep" ]; then
  cp "$keep_gitkeep" internal/web/web_dist/.gitkeep
  rm -f "$keep_gitkeep"
fi

# 暂存之后工作区必须仍然干净 —— 否则 Go 会把 vcs.modified=true 编进
# 产物（.gitkeep 就是这样一个坑：删掉它工作区就脏了）。
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
  echo "暂存前端后工作区变脏，产物会被标记为 dirty：" >&2
  git status --short >&2
  exit 1
fi

# 断言真的暂存进去了。这一步是防「npm run build 静默产出空目录」之类，
# 比等到用户发现 404 再排查便宜得多。
if [ ! -s internal/web/web_dist/index.html ]; then
  echo "前端暂存失败：internal/web/web_dist/index.html 不存在或为空" >&2
  echo "（这会让发布产物的 / 与 /accounts 全部 404）" >&2
  exit 1
fi
# 顺带确认版本号真的注入了（build.mjs 读仓库根 VERSION）。
if ! grep -rqs "$version" internal/web/web_dist/_next/static/chunks/; then
  echo "警告：前端产物里找不到版本串 ${version}，界面可能显示 0.0.0" >&2
fi

rm -rf "$out_dir"
mkdir -p "$out_dir"

# 与 release.yml 的 matrix 一致：每项三列 goos<TAB>goarch<TAB>goarm（空=不设）。
#
# 用 TAB 分隔而不是空格：早先用空格 + 引号占位（`linux arm '' 7`）时，
# `read` 会把 '' 与 7 一起塞进第三个变量，产出 `..._linux_armv 7` 这种
# 带空格的文件名 —— 一个只在 armv7 上出现的低级错误。
targets=(
  $'linux\tamd64\t'
  $'linux\tarm64\t'
  $'linux\tarm\t7'
  $'darwin\tamd64\t'
  $'darwin\tarm64\t'
  $'windows\tamd64\t'
)

echo "构建 chatgpt2api ${version} → ${out_dir}/"
for entry in "${targets[@]}"; do
  IFS=$'\t' read -r goos goarch goarm <<<"$entry"

  ext=""
  [ "$goos" = "windows" ] && ext=".exe"

  name="chatgpt2api_${version}_${goos}_${goarch}"
  [ -n "$goarm" ] && name="${name}v${goarm}"

  # -trimpath 去构建机路径；-s -w 去符号表；
  # -X 注入版本号（单二进制旁边没有 VERSION 文件，AppVersion 会回退到它）。
  ldflags="-s -w -X github.com/IFuZYI/chatgpt2api/internal/config.BuildVersion=${version}"

  GOOS="$goos" GOARCH="$goarch" GOARM="$goarm" CGO_ENABLED=0 \
    go build -trimpath -ldflags="$ldflags" -o "${out_dir}/${name}${ext}" ./cmd/server

  printf '  %-44s %s\n' "${name}${ext}" "$(du -h "${out_dir}/${name}${ext}" | cut -f1)"
done

# 校验和（与 release.yml 的 release job 同款命令）。
( cd "$out_dir" && sha256sum chatgpt2api_* > SHA256SUMS.txt )
echo
echo "校验和："
sed 's/^/  /' "${out_dir}/SHA256SUMS.txt"
echo
echo "产物已就绪：${out_dir}/"
echo "把这些文件上传到 tag ${version} 的 GitHub Release。"
