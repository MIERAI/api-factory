#!/usr/bin/env bash
# 式样书审查: AI 只读式样书/proto/schema,输出规格漏洞(JSON)。不给任何工具。
#   usage: scripts/review.sh <spec.yaml> [model] [label]   (model 省略 = factory.yaml 的默认)
#   EXPERIMENT=1: 对比实验用,允许策略以外的模型(例如用本地模型做审查)
#   退出码: 0 = 通过,1 = 有挡流水线的问题(流水线应在生成前停下),2 = 出错
#   轮次: 同一个 API(+标签)上次通过之后连续没通过的次数 + 1(factory review-stats 统计)
set -uo pipefail
SPEC=${1:?spec.yaml}; MODEL=${2:-}; LABEL=${3:-}   # MODEL 省略 = factory.yaml 的默认
mkdir -p .factory/reviews
API_ID=$(go run ./factory/cmd/factory ai-paths "$SPEC" | head -1)
OUT=.factory/reviews/$(date +%Y%m%d-%H%M%S)-${MODEL:-default}-$API_ID${LABEL:+-$LABEL}
mkdir -p "$OUT"
go run ./factory/cmd/factory review-prompt --label "$LABEL" --out "$OUT" "$SPEC" > "$OUT/prompt.md" || exit 2

t0=$(date +%s)
# 模型和调用方式在 factory.yaml(ai.roles.review);审查只允许强模型
go run ./factory/cmd/factory ai --role review ${MODEL:+--model "$MODEL"} ${EXPERIMENT:+--experiment} -- --output-format json < "$OUT/prompt.md" > "$OUT/ai.json" 2> "$OUT/ai.err" || { cat "$OUT/ai.err"; exit 2; }
sec=$(( $(date +%s) - t0 ))

# 解析 + 收敛规则(factory/review): 第 2 轮以后,重复提问丢弃、严重度不升级、上一轮就能问的新 major 记为"迟到"
go run ./factory/cmd/factory review-result "$OUT" "$sec"
