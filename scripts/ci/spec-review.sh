#!/usr/bin/env bash
# 阶段 1: 式样书的 PR(只改 design/)。机械检查 → 确定性生成(不提交)→ AI 审查改动的 API。
#   有挡流水线的问题 → 失败(PR 上回答问题: factory answer design/…,再推)
#   BASE: 和哪个提交比(GitHub: origin/main)。REVIEW=skip: 不调用 AI(本地模拟 / 没有 AI 凭证时)
#   REVIEW_MODEL: factory.yaml 的模型(空 = 默认 claude)。claude* 以外 = 只作参考: 意见贴出来,不挡 PR
#   (实测: 本地模型的 blocker 全是误报,审查后面没有闸门兜底)
source "$(dirname "$0")/lib.sh"
BASE=${BASE:-origin/main}
md "## 式样书检查" "" "| 步骤 | 时间 |" "|---|---|"
stage "① 式样书闸门(机械检查)" make --no-print-directory check
stage "② 确定性生成(只检查能不能生成)" make --no-print-directory prepare
# 要审查的 API = 审查的输入(设计书 + 审查记录 + 领域里它引用的部分)和 BASE 不同的。
# 领域.yaml 里加了一张无关的表 → 不审查(以前领域.yaml 一变,这个领域的全部 API 都重审;审查不确定,已经通过的设计书又被挡)
specs=$(go run ./factory/cmd/factory review-changed -v --base "$BASE")
md "" "改动的 API: $(echo $specs | wc -w | tr -d ' ') 个" ""
blocked=0
for s in $specs; do
  id=$(go run ./factory/cmd/factory ai-paths "$s" | head -1)
  if [[ ${REVIEW:-} == skip ]]; then
    md "- \`$id\`: AI 审查跳过(REVIEW=skip)"
    continue
  fi
  # 审查的输入(式样书 / 设计书 / 审查记录 / 领域 / prompt / 模型)和上次通过时相同 → 沿用(审查是不确定的:
  # 同一个设计书再审一次可能翻转;.factory/reviews 在 CI 里按 PR 分支保留,见 spec-pr.yml)
  key=$(scripts/ci/review-key.sh "$s" "${REVIEW_MODEL:-default}")
  mark=.factory/reviews/passed/$id-$key
  if [[ -f $mark ]]; then
    md "- \`$id\`: ✓ 审查通过(审查的输入没有变,沿用 $(cat "$mark") 的结果)"
    continue
  fi
  rc=0; out=$(scripts/review.sh "$s" "${REVIEW_MODEL:-}" 2>&1) || rc=$?
  if [[ $rc == 0 ]]; then mkdir -p .factory/reviews/passed; date '+%Y-%m-%d %H:%M' > "$mark"; fi
  usage=$(scripts/ci/ai-usage.sh "$(ls -td .factory/reviews/*-"${REVIEW_MODEL:-default}"-"$id"* 2>/dev/null | head -1)/ai.json")
  if [[ -n ${REVIEW_MODEL:-} && ${REVIEW_MODEL} != claude* ]]; then   # 只作参考
    case $rc in
      0) md "- \`$id\`: ✓ 审查通过($REVIEW_MODEL,参考)" ;;
      1) md "- \`$id\`: ⚠ $REVIEW_MODEL 的审查意见(仅供参考,不阻塞;本地模型误报多,由人判断)" "" '```' "$out" '```' ;;
      *) md "- \`$id\`: ⚠ 审查工具出错(不阻塞)" "" '```' "$out" '```' ;;
    esac
    md "  - $usage"
    continue
  fi
  case $rc in
    0) md "- \`$id\`: ✓ 审查通过" ;;
    1) md "- \`$id\`: ✗ 有要回答的问题" "" '```' "$out" '```'; blocked=1 ;;
    *) md "- \`$id\`: ⚠ 审查工具出错(不是规格的问题)" "" '```' "$out" '```'; blocked=1 ;;
  esac
  md "  - $usage"
done
exit $blocked
