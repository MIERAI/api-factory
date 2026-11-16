#!/usr/bin/env bash
# 整条流水线:
#   ① 式样书闸门(机械检查) → ② 确定性生成 → ③ 式样书审查(AI,看式样书 + 生成的查询;有规格漏洞就停)
#   → ④ AI 实现(只有 compute/custom) → ⑤ 路径/纯函数闸门 → ⑥ 编译 + 验收
# 每一步计时,结果写到 .factory/loop-<时间>.log
#   usage: scripts/loop.sh <spec.yaml> [claude|local]
set -euo pipefail
SPEC=${1:?usage: loop.sh <spec.yaml> [claude|local]}
MODEL=${2:-claude}
mkdir -p .factory
LOG=.factory/loop-$(date +%Y%m%d-%H%M%S).log
exec > >(tee "$LOG") 2>&1

T0=$(date +%s)
step() { local s=$(date +%s); echo; echo "━━ $1"; shift; "$@"; echo "   ⏱  $(( $(date +%s) - s ))s"; }
F="go run ./factory/cmd/factory"

step "① 式样书闸门" make --no-print-directory check

step "② 确定性生成" make --no-print-directory prepare

echo; echo "━━ ③ 式样书审查"
if ! ./scripts/review.sh "$SPEC" "$MODEL"; then
  echo
  echo "✗ 式样书审查发现了规格漏洞,在 AI 实现前停止。"
  echo "  处理方法: 把决定写进式样书 behavior / acceptance,或记录到 review_decisions,然后重新执行。"
  exit 1
fi

if [[ $($F ai-paths "$SPEC" | wc -l) -gt 1 ]]; then
  # 基线: AI 动手前的状态,路径闸门以此为准
  git add -A
  git -c user.name=factory -c user.email=factory@localhost commit -q -m "factory: prepare $(basename "$(dirname "$SPEC")")" --allow-empty
  allowed='Read,Glob,Grep,Write,Edit,Bash(go build:*),Bash(go vet:*),Bash(go test:*)'
  if [[ $MODEL == local ]]; then
    ai() { env ANTHROPIC_BASE_URL="${LOCAL_URL:-http://100.106.140.35:11434}" ANTHROPIC_AUTH_TOKEN=ollama ANTHROPIC_API_KEY= \
      ANTHROPIC_DEFAULT_HAIKU_MODEL="${LOCAL_MODEL:-qwen3.6:35b}" ANTHROPIC_DEFAULT_SONNET_MODEL="${LOCAL_MODEL:-qwen3.6:35b}" \
      ANTHROPIC_DEFAULT_OPUS_MODEL="${LOCAL_MODEL:-qwen3.6:35b}" CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
      claude -p --model "${LOCAL_MODEL:-qwen3.6:35b}" --allowedTools "$allowed" --max-turns 40 --output-format text; }
  else
    ai() { claude -p --allowedTools "$allowed" --max-turns 40 --output-format text; }
  fi
  step "④ AI 实现 ($MODEL)" bash -c "$(declare -f ai); allowed='$allowed'; $F prompt '$SPEC' | ai"
  step "⑤ 路径/纯函数闸门" make --no-print-directory guard
else
  echo; echo "━━ ④⑤ 跳过: pattern 由工厂完整生成,AI 不参与"
fi

step "⑥ 编译 + 验收" make --no-print-directory accept ACCEPT_SPECS="$(dirname "$SPEC")"

echo; echo "✓ 流水线完成,总耗时 $(( $(date +%s) - T0 ))s,日志: $LOG"
