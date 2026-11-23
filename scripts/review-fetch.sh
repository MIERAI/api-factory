#!/usr/bin/env bash
# CI(spec-pr)的审查结果下载到本地的 .factory/reviews → 本地就能 factory answer F1=… 回答
#   scripts/review-fetch.sh                  当前分支的 PR 最新一次 spec-pr 运行
#   scripts/review-fetch.sh --pr 25          PR #25
#   scripts/review-fetch.sh --branch design/x
#   scripts/review-fetch.sh --run 123456789  指定的运行
# 用 gh(GH_TOKEN 可以; 仓库默认 = origin,REPO=owner/name 可以覆盖)。只追加: 本地已有的审查目录不覆盖。
# 本仓库和项目(<项目>/.factory/reviews)都下载;之后 factory review-sync 把 CI 的轮次写进轮次记录(X.审查轮次.jsonl,提交)。
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
# artifact 的布局: 新 = 仓库根开始的路径(.factory/reviews/… 和 <项目>/.factory/reviews/…);
# 旧(只有本仓库)/ 只有本仓库的结果时 = .factory/reviews 的内容直接在根上
roots=()
while IFS= read -r d; do roots+=("${d#"$tmp"/}"); done < <(find "$tmp" -type d -path '*/.factory/reviews' | sort)
[[ ${#roots[@]} -gt 0 ]] || roots=(".")
n=0
for r in "${roots[@]}"; do
  src=$tmp/$r; dst=$r
  [[ $r == . ]] && dst=.factory/reviews
  mkdir -p "$dst"
  for d in "$src"/*/; do
    [[ -d $d ]] || continue
    name=$(basename "$d")
    if [[ $name == passed ]]; then   # "通过"的标记(按指纹沿用): 合并
      mkdir -p "$dst/passed"; cp -n "$d"* "$dst/passed/" 2>/dev/null || true; continue
    fi
    [[ -e $dst/$name ]] && continue
    cp -R "$d" "$dst/$name"; n=$((n + 1))
  done
done
echo "✓ 运行 $run 的审查结果 → .factory/reviews(新增 $n 个)"
# 轮次记录(design/<领域>/X.审查轮次.jsonl,提交): CI 的轮次写进去 → 提交后本地和 CI 共用一份历史
for p in . $(scripts/projects.sh --list); do
  if [[ $p == . ]]; then go run ./factory/cmd/factory review-sync; else go run ./factory/cmd/factory --project "$p" review-sync | sed "s#^✓ #✓ $p/#"; fi
done
# 每个 API 最新的一轮(目录名 = 时间戳-模型-API[-标签])还没通过的: 要回答的
for p in . $(scripts/projects.sh --list); do
  [[ -d $p/.factory/reviews ]] || continue
  for d in $(ls -d "$p"/.factory/reviews/*/ 2>/dev/null | sort); do
    b=$(basename "$d"); echo "${b#*-*-} $d"
  done | awk '{last[$1]=$2} END {for (k in last) print last[k]}' | sort | while read -r d; do
    [[ -f $d/verdict.json ]] && grep -q '"passed": false' "$d/verdict.json" && echo "  ✗ 没通过: ${d%/.factory/reviews/*}: $(basename "$d")"
  done
done
echo "回答: go run ./factory/cmd/factory answer [<项目>/]design/<领域>/<API>.yaml   (或 F1=2 / F1=keep:\"现在的行为\" …,--list 只列出)"
echo "然后提交 X.审查.yaml 和 X.审查轮次.jsonl(轮次记录)"
