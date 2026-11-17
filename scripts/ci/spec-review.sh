#!/usr/bin/env bash
# 阶段 1: 式样书的 PR(只改 design/)。机械检查 → 确定性生成(不提交)→ AI 审查改动的 API。
#   有挡流水线的问题 → 失败(PR 上回答问题: factory answer design/…,再推)
#   BASE: 和哪个提交比(GitHub: origin/main)。REVIEW=skip: 不调用 AI(本地模拟 / 没有 AI 凭证时)
source "$(dirname "$0")/lib.sh"
BASE=${BASE:-origin/main}
md "## 式样书检查" "" "| 步骤 | 时间 |" "|---|---|"
stage "① 式样书闸门(机械检查)" make --no-print-directory check
stage "② 确定性生成(只检查能不能生成)" make --no-print-directory prepare
specs=$(changed_specs "$BASE")
md "" "改动的 API: $(echo $specs | wc -w | tr -d ' ') 个" ""
blocked=0
for s in $specs; do
  id=$(go run ./factory/cmd/factory ai-paths "$s" | head -1)
  if [[ ${REVIEW:-} == skip ]]; then
    md "- \`$id\`: AI 审查跳过(REVIEW=skip)"
    continue
  fi
  rc=0; out=$(scripts/review.sh "$s" 2>&1) || rc=$?
  case $rc in
    0) md "- \`$id\`: ✓ 审查通过" ;;
    1) md "- \`$id\`: ✗ 有要回答的问题" "" '```' "$out" '```'; blocked=1 ;;
    *) md "- \`$id\`: ⚠ 审查工具出错(不是规格的问题)" "" '```' "$out" '```'; blocked=1 ;;
  esac
done
exit $blocked
