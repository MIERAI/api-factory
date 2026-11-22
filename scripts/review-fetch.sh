#!/usr/bin/env bash
# CI(spec-pr)的审查结果下载到本地的 .factory/reviews → 本地就能 factory answer F1=… 回答
#   scripts/review-fetch.sh                  当前分支的 PR 最新一次 spec-pr 运行
#   scripts/review-fetch.sh --pr 25          PR #25
#   scripts/review-fetch.sh --branch design/x
#   scripts/review-fetch.sh --run 123456789  指定的运行
# 用 gh(GH_TOKEN 可以; 仓库默认 = origin,REPO=owner/name 可以覆盖)。只追加: 本地已有的审查目录不覆盖。
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
pr= branch= run=
while (( $# )); do
  case $1 in
    --pr) pr=$2; shift 2 ;;
    --branch) branch=$2; shift 2 ;;
    --run) run=$2; shift 2 ;;
    -h|--help) sed -n '2,8p' "$0"; exit 0 ;;
    *) echo "不认识的参数: $1" >&2; exit 2 ;;
  esac
done
repo=${REPO:-$(gh repo view --json nameWithOwner -q .nameWithOwner)}
if [[ -z $run ]]; then
  if [[ -n $pr ]]; then branch=$(gh pr view "$pr" -R "$repo" --json headRefName -q .headRefName); fi
  branch=${branch:-$(git branch --show-current)}
  [[ -n $branch ]] || { echo "✗ 不在分支上: 用 --pr / --branch 指定" >&2; exit 2; }
  # 最新的已完成的运行(失败的也算: 有问题要回答的运行就是失败的)
  read -r run sha < <(gh run list -R "$repo" --workflow spec-pr.yml --branch "$branch" --status completed --limit 1 \
    --json databaseId,headSha -q '.[0] | "\(.databaseId) \(.headSha)"')
  [[ -n ${run:-} ]] || { echo "✗ $branch 上没有完成的 spec-pr 运行" >&2; exit 1; }
  if [[ $sha != "$(git rev-parse "origin/$branch" 2>/dev/null || true)" && $sha != "$(git rev-parse HEAD)" ]]; then
    echo "⚠ 运行 $run 审查的是 ${sha:0:7}(和本地的 HEAD / origin/$branch 不同: 还有更新的推送在跑?)" >&2
  fi
fi
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
gh run download "$run" -R "$repo" -n spec-reviews -D "$tmp" || {
  echo "✗ 运行 $run 没有 spec-reviews 的 artifact(这个功能之前的运行 / 已过保留期)" >&2; exit 1; }
mkdir -p .factory/reviews
n=0
for d in "$tmp"/*/; do
  name=$(basename "$d")
  if [[ $name == passed ]]; then   # "通过"的标记(按指纹沿用): 合并
    mkdir -p .factory/reviews/passed; cp -n "$d"* .factory/reviews/passed/ 2>/dev/null || true; continue
  fi
  [[ -e .factory/reviews/$name ]] && continue
  cp -R "$d" ".factory/reviews/$name"; n=$((n + 1))
done
echo "✓ 运行 $run 的审查结果 → .factory/reviews(新增 $n 个)"
# 每个 API 最新的一轮(目录名 = 时间戳-模型-API[-标签])还没通过的: 要回答的
for d in $(ls -d .factory/reviews/*/ | sort); do
  b=$(basename "$d"); echo "${b#*-*-} $d"
done | awk '{last[$1]=$2} END {for (k in last) print last[k]}' | sort | while read -r d; do
  [[ -f $d/verdict.json ]] && grep -q '"passed": false' "$d/verdict.json" && echo "  ✗ 没通过: $(basename "$d")"
done
echo "回答: go run ./factory/cmd/factory answer design/<领域>/<API>.yaml   (或 F1=2 …,--list 只列出)"
