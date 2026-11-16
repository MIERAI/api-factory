#!/usr/bin/env bash
# 重构的安全网: 重新生成后,和快照逐字节比较。重构只改内部结构,不改输出。
#   scripts/golden-diff.sh <golden.tar>
set -euo pipefail
G=${1:?golden.tar}
T=$(mktemp -d)
tar xf "$G" -C "$T"
rc=0
while IFS= read -r f; do
  [[ -f "$T/$f" ]] || continue
  if ! cmp -s "$T/$f" "$f"; then echo "  ✗ 变了: $f"; rc=1; fi
done < <(tar tf "$G")
for f in $(ls gen/acceptance/*.yaml db/queries/*.sql internal/handler/*/*_gen*.go 2>/dev/null); do
  [[ -f "$T/$f" ]] || { echo "  ✗ 多了: $f"; rc=1; }
done
rm -rf "$T"
[[ $rc -eq 0 ]] && echo "✓ 生成结果与快照完全一致"
exit $rc
