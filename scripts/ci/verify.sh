#!/usr/bin/env bash
# 阶段 3: 生成物的 PR。生成物是不是最新的 → 编译 / 单元测试 → 验收(3 个种子)→ 变异余量。全部通过才能合并。
source "$(dirname "$0")/lib.sh"
md "## 验证" "" "| 步骤 | 时间 |" "|---|---|"
stage "① 式样书闸门" make --no-print-directory check
uptodate() {
  make --no-print-directory prepare >/dev/null
  if ! git diff --quiet || [[ -n $(git ls-files --others --exclude-standard) ]]; then
    echo "✗ 生成物不是最新的(式样书和生成的代码不一致): 重新运行 make design prepare 再提交"
    git status --short | head -20
    return 1
  fi
}
stage "② 生成物是最新的" uptodate
stage "③ 编译 + 单元测试 + 验收(3 个种子)" bash -c "make --no-print-directory accept SEEDS=',s1,s2' 2>&1 | grep -v '^ok\|no test files' | tee .factory/accept.txt; exit \${PIPESTATUS[0]}"
stage "④ 变异余量(margin)" bash -c "make --no-print-directory margin 2>&1 | tee .factory/margin.txt; exit \${PIPESTATUS[0]}"
md "" '```' "$(grep -E '✓|✗|seed=' .factory/accept.txt .factory/margin.txt | sed 's/^.factory\///' | tail -12)" '```'
