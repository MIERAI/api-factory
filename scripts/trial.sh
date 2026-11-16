#!/usr/bin/env bash
# 对照试验: 同一份式样书 × 同一个 harness(claude -p) × 不同模型,重复 N 次
#   usage: scripts/trial.sh <claude|local> <spec.yaml> <N>
# 结果追加到 .factory/trials/results.jsonl,每次的全部输出留在 .factory/trials/<run>/
set -uo pipefail
MODEL=${1:?claude|local}; SPEC=${2:?spec.yaml}; N=${3:-3}
ROOT=$(pwd)
DB_URL=${DB_URL:-postgres://factory:factory@127.0.0.1:55432/factory?sslmode=disable}
LOCAL_MODEL=${LOCAL_MODEL:-qwen3.6:35b}
LOCAL_URL=${LOCAL_URL:-http://100.106.140.35:11434}
AI_TIMEOUT=${AI_TIMEOUT:-1800}
mkdir -p .factory/trials .factory/wt
go build -o .factory/factory ./factory/cmd/factory || exit 1
F="$ROOT/.factory/factory"
API_ID=$($F ai-paths "$SPEC" | head -1)
AI_FILES=$($F ai-paths "$SPEC" | tail -n +2)

run_ai() {  # $1 = prompt file, $2 = json out
  local allowed='Read,Glob,Grep,Write,Edit,Bash(go build:*),Bash(go vet:*),Bash(go test:*)'
  if [[ $MODEL == local ]]; then
    env ANTHROPIC_BASE_URL="$LOCAL_URL" ANTHROPIC_AUTH_TOKEN=ollama ANTHROPIC_API_KEY= \
      ANTHROPIC_DEFAULT_HAIKU_MODEL="$LOCAL_MODEL" ANTHROPIC_DEFAULT_SONNET_MODEL="$LOCAL_MODEL" \
      ANTHROPIC_DEFAULT_OPUS_MODEL="$LOCAL_MODEL" CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
      perl -e 'alarm shift; exec @ARGV' "$AI_TIMEOUT" \
      claude -p --model "$LOCAL_MODEL" --allowedTools "$allowed" --max-turns 40 --output-format json < "$1" > "$2" 2>/dev/null
  else
    perl -e 'alarm shift; exec @ARGV' "$AI_TIMEOUT" \
      claude -p --allowedTools "$allowed" --max-turns 40 --output-format json < "$1" > "$2" 2>/dev/null
  fi
}

for i in $(seq 1 "$N"); do
  RUN="$(date +%Y%m%d-%H%M%S)-$MODEL-$API_ID-$i"
  OUT="$ROOT/.factory/trials/$RUN"; WT="$ROOT/.factory/wt/$RUN"
  mkdir -p "$OUT"
  git worktree add -q --detach "$WT" HEAD
  cd "$WT"
  # 重置到 AI 动手前: 删掉该 API 由 AI 负责的文件
  for f in $AI_FILES; do rm -f "$f"; done
  git add -A && git -c user.name=trial -c user.email=trial@localhost commit -q -m "trial baseline" --allow-empty

  $F prompt "$SPEC" > "$OUT/prompt.md"
  t0=$(date +%s); run_ai "$OUT/prompt.md" "$OUT/ai.json"; ai_rc=$?; ai_sec=$(( $(date +%s) - t0 ))
  grep '^{' "$OUT/ai.json" | tail -1 > "$OUT/ai.result.json" 2>/dev/null

  $F guard > "$OUT/guard.txt" 2>&1; guard_rc=$?
  { go build -o bin/server ./cmd/server && go vet ./...; } > "$OUT/build.txt" 2>&1; build_rc=$?
  accept_pass=0; accept_total=0
  if [[ $build_rc -eq 0 ]]; then
    $F accept --server bin/server --db "$DB_URL" > "$OUT/accept.txt" 2>&1
    accept_total=$(grep -c "\[$API_ID\]" "$OUT/accept.txt")
    accept_pass=$(grep "\[$API_ID\]" "$OUT/accept.txt" | grep -c '✓')
  fi
  git status --porcelain --untracked-files=all > "$OUT/changed.txt"
  git diff HEAD > "$OUT/diff.patch"; for f in $(git ls-files --others --exclude-standard); do echo "=== $f"; cat "$f"; done >> "$OUT/diff.patch"

  python3 - "$OUT" "$MODEL" "$API_ID" "$i" "$ai_rc" "$ai_sec" "$guard_rc" "$build_rc" "$accept_pass" "$accept_total" >> "$ROOT/.factory/trials/results.jsonl" <<'PY'
import json, sys
out, model, api, i, ai_rc, ai_sec, guard_rc, build_rc, ap, at = sys.argv[1:]
try:
    r = json.load(open(f"{out}/ai.result.json"))
except Exception:
    r = {}
rec = dict(run=out.rsplit("/", 1)[-1], model=model, api=api, trial=int(i),
           ai_rc=int(ai_rc), ai_sec=int(ai_sec), turns=r.get("num_turns"),
           ai_status=r.get("subtype"), guard_ok=guard_rc == "0", build_ok=build_rc == "0",
           accept=f"{ap}/{at}", passed=build_rc == "0" and guard_rc == "0" and ap == at and at != "0")
print(json.dumps(rec, ensure_ascii=False))
PY
  tail -1 "$ROOT/.factory/trials/results.jsonl"
  cd "$ROOT"; git worktree remove --force "$WT"
done
