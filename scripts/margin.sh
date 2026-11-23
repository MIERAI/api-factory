#!/usr/bin/env bash
# 差分测试的余量(P2 的收敛标准): 换几个随机种子跑变异,每个变异都要被 ≥ MIN 个差分用例抓到。
# 例外: 变异名里写了 [单元测试负责] 的(差分测试原理上测不到,例如违反 CHECK 约束的数据)。
#   usage: scripts/margin.sh [seed ...]     (默认: "" s1 s2;MARGIN_SEED_COUNT=N: 前 N 个)
set -uo pipefail
MIN=${MIN:-3}
# 并行执行(scripts/mutate-par.py): 变异编进一个服务器(合编)/ go build -overlay(工作区不动),每个 worker 一套数据库。
# 没有参数时种子由 mutate-par.py 决定(MARGIN_SEED_COUNT=1 → 只有默认的种子;CI 的 PR 用)。串行的旧方式: SERIAL=1
if [[ -z ${SERIAL:-} ]]; then
  exec python3 "$(dirname "$0")/mutate-par.py" --min "$MIN" ${WORKERS:+--workers "$WORKERS"} "$@"
fi
seeds=("$@"); [[ ${#seeds[@]} -eq 0 ]] && seeds=("" s1 s2)
fail=0
for seed in "${seeds[@]}"; do
  out=$(FACTORY_DIFF_SEED="$seed" "$(dirname "$0")/mutate.sh" 2>&1)
  expected=$(grep -c '^run "' "$(dirname "$0")/mutate.sh")
  echo "$out" | python3 "$(dirname "$0")/margin.py" "$seed" "$MIN" "$expected" || fail=1
done
exit $fail
