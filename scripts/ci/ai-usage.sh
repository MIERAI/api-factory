#!/usr/bin/env bash
# claude -p --output-format json 的结果 → 一行: 实际用的模型、token、费用、时间(CI 的摘要 / PR 评论用)
#   scripts/ci/ai-usage.sh <ai.json>     读不了 → "(用量不明)"
python3 -I - "$1" <<'PY' 2>/dev/null || echo "(用量不明)"
import json, sys
d = json.load(open(sys.argv[1]))
mu = d.get("modelUsage") or {}
models = ", ".join(sorted(mu)) or "?"
tin = sum(v.get("inputTokens", 0) + v.get("cacheReadInputTokens", 0) + v.get("cacheCreationInputTokens", 0) for v in mu.values())
tout = sum(v.get("outputTokens", 0) for v in mu.values())
cost = d.get("total_cost_usd")
dur = (d.get("duration_ms") or 0) / 1000
print(f"模型 {models} · 输入 {tin:,} / 输出 {tout:,} tokens · " + (f"${cost:.3f}(按 API 价格换算;订阅时是额度)" if cost is not None else "费用不明") + f" · {dur:.0f}s · {d.get('num_turns', '?')} 轮")
PY
