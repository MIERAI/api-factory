#!/usr/bin/env bash
# 整条循环: 式样书闸门 → 确定性生成 → AI 实现 → 路径闸门 → 编译 → 验收
# 每一步计时,结果写到 .factory/loop-<时间>.log
set -euo pipefail
SPEC=${1:?usage: loop.sh <spec.yaml>}
mkdir -p .factory
LOG=.factory/loop-$(date +%Y%m%d-%H%M%S).log
exec > >(tee "$LOG") 2>&1

T0=$(date +%s)
step() { local s=$(date +%s); echo; echo "━━ $1"; shift; "$@"; echo "   ⏱  $(( $(date +%s) - s ))s"; }

step "① 式样书闸门" make --no-print-directory check
step "② 确定性生成" make --no-print-directory prepare

# 基线: AI 动手前的状态,路径闸门以此为准
git add -A
git -c user.name=factory -c user.email=factory@localhost commit -q -m "factory: prepare $(basename "$(dirname "$SPEC")")" --allow-empty

step "③ AI 实现" bash -c "go run ./factory/cmd/factory prompt '$SPEC' | claude -p \
  --allowedTools 'Read,Glob,Grep,Write,Edit,Bash(go build:*),Bash(go vet:*),Bash(go test:*)' \
  --output-format text"

step "④ 路径闸门" make --no-print-directory guard
step "⑤ 编译 + 验收" make --no-print-directory accept

echo; echo "✓ 循环完成,总耗时 $(( $(date +%s) - T0 ))s,日志: $LOG"
