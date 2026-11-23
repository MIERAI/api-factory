#!/usr/bin/env bash
# 式样书审查: AI 只读式样书/proto/schema,输出规格漏洞(JSON)。不给任何工具。
#   usage: scripts/review.sh <spec.yaml> [model] [label]   (model 省略 = factory.yaml 的默认)
#   PROJECT=examples/x/catalog: 项目(factory --project)的式样书(spec.yaml 是项目里的路径;结果在 <项目>/.factory/reviews)
#   EXPERIMENT=1: 对比实验用,允许策略以外的模型(例如用本地模型做审查)
#   退出码: 0 = 通过,1 = 有挡流水线的问题(流水线应在生成前停下),2 = 出错
#   轮次: 同一个 API(+标签)上次通过之后连续没通过的次数 + 1(审查目录 + 提交的轮次记录 X.审查轮次.jsonl,factory/review/rounds.go)
set -uo pipefail
SPEC=${1:?spec.yaml}; MODEL=${2:-}; LABEL=${3:-}   # MODEL 省略 = factory.yaml 的默认
F() { go run ./factory/cmd/factory ${PROJECT:+--project "$PROJECT"} "$@"; }
ROOT=${PROJECT:-.}/.factory/reviews
mkdir -p "$ROOT"
ROOT=$(cd "$ROOT" && pwd)   # 绝对路径: factory --project 会切换到项目目录
API_ID=$(F ai-paths "$SPEC" | head -1)
# 目录名 = 轮次的 ID: 时间用 UTC(本地和 CI 的轮次合在一起按时间排序)
OUT=$ROOT/$(TZ=UTC date +%Y%m%d-%H%M%S)-${MODEL:-default}-$API_ID${LABEL:+-$LABEL}
mkdir -p "$OUT"
F review-prompt --label "$LABEL" --out "$OUT" "$SPEC" > "$OUT/prompt.md" || exit 2

t0=$(date +%s)
# 模型和调用方式在 factory.yaml(ai.roles.review);审查只允许强模型
F ai --role review ${MODEL:+--model "$MODEL"} ${EXPERIMENT:+--experiment} -- --output-format json < "$OUT/prompt.md" > "$OUT/ai.json" 2> "$OUT/ai.err" || { cat "$OUT/ai.err"; exit 2; }
sec=$(( $(date +%s) - t0 ))

# 解析 + 收敛规则(factory/review): 已经回答过的不再挡、严重度不升级、迟到、轮次封顶;写轮次记录
F review-result "$OUT" "$sec"
