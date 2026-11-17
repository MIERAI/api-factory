#!/usr/bin/env bash
# 生成物 PR 的说明: 管理者看这一页(不用逐行读生成的代码)。
source "$(dirname "$0")/lib.sh"
BASE=$1; SPECS=$2
echo "## 生成物"
echo
echo "式样书: $(git log --format='%h %s' -1 "$BASE"..HEAD~1 2>/dev/null | head -1)"
echo
echo "| API | 种类 | 输入 | AI 写的文件 |"
echo "|---|---|---|---|"
for s in $SPECS; do
  id=$(go run ./factory/cmd/factory ai-paths "$s" | head -1)
  kind=$(grep -m1 '^    kind:' "$s" | awk '{print $2}')
  inputs=$(awk '/^    filters:/{f=1;next} f&&/^        [a-z_]+:$/{gsub(":","");printf "%s ",$1} f&&/^    [a-z]/{f=0}' "$s")
  n=$(( $(go run ./factory/cmd/factory ai-paths "$s" | wc -l) - 1 ))
  echo "| \`$id\` | $kind | ${inputs:-–} | $n |"
done
echo
echo "### 要看的"
echo "- 式样书本身已经在式样书的 PR 里审查过(AI 审查 + 人)。这里看: AI 写的文件(上表不是 0 的)、CI 的验收 / 变异的结果"
echo "- 生成的查询(db/queries/*.sql)、handler、proto 是工厂确定性生成的,工厂的测试保证;不用逐行读"
echo
echo "### 改动的文件"
echo '```'
git diff --stat "$BASE" HEAD -- . ':!design' | tail -25
echo '```'
