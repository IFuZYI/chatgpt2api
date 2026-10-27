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
allow_dirty=false
if [ "${1:-}" = "--allow-dirty" ]; then
  allow_dirty=true
  shift
  out_dir="${1:-dist-release}"
fi
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
