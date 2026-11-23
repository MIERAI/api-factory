#!/usr/bin/env bash
# 阶段 3 的验收,一个种子(CI 的 matrix: default / s1 / s2 三个并行 job;合起来 = verify.sh 的"验收 3 个种子",判定不变)。
#   ACCEPT_SEED=default|s1|s2
source "$(dirname "$0")/lib.sh"
: "${ACCEPT_SEED:?ACCEPT_SEED=default|s1|s2}"
seed=$ACCEPT_SEED; [[ $seed == default ]] && seed=""
md "## 验收 种子 $ACCEPT_SEED" "" "| 步骤 | 时间 |" "|---|---|"
# 干净的 checkout(CI 的独立 job)里还没有 .factory 的生成物: 先 prepare(margin 分片曾经忘了这一步,全部"执行器出错")
stage "② 生成(prepare)" make --no-print-directory prepare
stage "③ 编译 + 单元测试 + 验收(种子 $ACCEPT_SEED)" bash -c "make --no-print-directory accept ${seed:+SEEDS=$seed} 2>&1 | grep -v '^ok\|no test files' | tee .factory/accept.txt; exit \${PIPESTATUS[0]}"
md "" '```' "$(grep -E '✓ acceptance|✗' .factory/accept.txt 2>/dev/null | tail -8)" '```'
