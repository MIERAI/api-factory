#!/usr/bin/env bash
# 式样书审查: AI 只读式样书/proto/schema,输出规格漏洞(JSON)。不给任何工具。
#   usage: scripts/review.sh <spec.yaml> [claude|local] [label]
#   退出码: 0 = 无 blocker/major,1 = 有 blocker/major(流水线应在生成前停下)
set -uo pipefail
SPEC=${1:?spec.yaml}; MODEL=${2:-claude}; LABEL=${3:-}
LOCAL_MODEL=${LOCAL_MODEL:-qwen3.6:35b}; LOCAL_URL=${LOCAL_URL:-http://100.106.140.35:11434}
mkdir -p .factory/reviews
API_ID=$(go run ./factory/cmd/factory ai-paths "$SPEC" | head -1)
OUT=.factory/reviews/$(date +%Y%m%d-%H%M%S)-$MODEL-$API_ID${LABEL:+-$LABEL}
mkdir -p "$OUT"
go run ./factory/cmd/factory review-prompt "$SPEC" > "$OUT/prompt.md" || exit 2

t0=$(date +%s)
if [[ $MODEL == local ]]; then
  env ANTHROPIC_BASE_URL="$LOCAL_URL" ANTHROPIC_AUTH_TOKEN=ollama ANTHROPIC_API_KEY= \
    ANTHROPIC_DEFAULT_HAIKU_MODEL="$LOCAL_MODEL" ANTHROPIC_DEFAULT_SONNET_MODEL="$LOCAL_MODEL" \
    ANTHROPIC_DEFAULT_OPUS_MODEL="$LOCAL_MODEL" CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    claude -p --model "$LOCAL_MODEL" --tools "" --max-turns 2 --output-format json < "$OUT/prompt.md" > "$OUT/ai.json" 2>/dev/null
else
  claude -p --tools "" --max-turns 2 --output-format json < "$OUT/prompt.md" > "$OUT/ai.json" 2>/dev/null
fi
sec=$(( $(date +%s) - t0 ))

python3 - "$OUT" "$sec" <<'PY'
import json, re, sys
out, sec = sys.argv[1], sys.argv[2]
raw = open(f"{out}/ai.json").read()
line = [l for l in raw.splitlines() if l.startswith("{")]
result = json.loads(line[-1]).get("result", "") if line else ""
m = re.findall(r"```json\s*(\{.*?\})\s*```", result, re.S) or re.findall(r"(\{\s*\"findings\".*\})", result, re.S)
try:
    findings = json.loads(m[-1])["findings"]
except Exception as e:
    print(f"✗ could not parse review output ({e}); see {out}/ai.json"); sys.exit(2)
json.dump(findings, open(f"{out}/findings.json", "w"), ensure_ascii=False, indent=2)
order = {"blocker": 0, "major": 1, "minor": 2}
findings.sort(key=lambda f: order.get(f.get("severity"), 3))
for f in findings:
    print(f"  [{f.get('severity','?'):7}] {f.get('id','')} {f.get('category','')} @ {f.get('location','')}")
    print(f"            {f.get('question','')}")
n = {k: sum(1 for f in findings if f.get("severity") == k) for k in order}
# 收敛规则: 只有"规格不明确"的 blocker/major 挡流水线;"验收覆盖"只是建议(审查员总能想出更多测试)
blocking = [f for f in findings if f.get("severity") in ("blocker", "major") and f.get("category") != "验收覆盖"]
advisory = [f for f in findings if f not in blocking and f.get("category") == "验收覆盖"]
print(f"⏱ {sec}s  blocker={n['blocker']} major={n['major']} minor={n['minor']}  "
      f"→ 挡流水线 {len(blocking)} 件 / 覆盖建议 {len(advisory)} 件  ({out}/findings.json)")
sys.exit(1 if blocking else 0)
PY
