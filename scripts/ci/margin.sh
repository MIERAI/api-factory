#!/usr/bin/env bash
# 阶段 3 的变异余量,一个分片(CI 的 matrix): MARGIN_SHARD=k/N → 清单里第 i 个变异 i % N == k 的那些,
# 3 个种子都跑,每个变异 ≥3 个差分用例(和 make margin 同一个判定;N 个分片合起来 = 全部变异)。
source "$(dirname "$0")/lib.sh"
: "${MARGIN_SHARD:?MARGIN_SHARD=k/N}"
md "## 变异余量 分片 $MARGIN_SHARD" "" "| 步骤 | 时间 |" "|---|---|"
stage "④ 变异余量(margin,分片 $MARGIN_SHARD)" bash -c "make --no-print-directory margin 2>&1 | tee .factory/margin.txt; exit \${PIPESTATUS[0]}"
md "" '```' "$(grep -E '✓|✗|seed=|⏱' .factory/margin.txt 2>/dev/null | tail -8)" '```'
