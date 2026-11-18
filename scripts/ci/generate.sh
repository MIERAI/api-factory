#!/usr/bin/env bash
# 阶段 2: 式样书合并到 main 之后。生成代码(确定性;计算 / 自定义的 API 由 AI 写纯函数)→ 生成物的分支 + PR 的说明。
#   BASE: 合并前的提交(GitHub: github.event.before)。IMPLEMENT_MODEL: factory.yaml 的模型(空 = 角色的默认)。输出: 分支 factory/gen-<sha>,PR 说明 .factory/pr-body.md
#   GitHub 上由 workflow 推送分支、gh pr create;本地模拟由 sim.sh 合并
source "$(dirname "$0")/lib.sh"
BASE=${BASE:?BASE=合并前的提交}
md "## 生成" "" "| 步骤 | 时间 |" "|---|---|"
stage "② 确定性生成" bash -c "make --no-print-directory design >/dev/null && make --no-print-directory prepare"
specs=$(changed_specs "$BASE")
ai_specs=()
rm -f .factory/ai-usage.md   # AI 的用量(实际的模型 / token / 费用)→ 生成物 PR 的说明
for s in $specs; do   # AI 要写的文件(计算 / 自定义)
  if [[ $(go run ./factory/cmd/factory ai-paths "$s" | wc -l) -gt 1 ]]; then ai_specs+=("$s"); fi
done
if (( ${#ai_specs[@]} )); then
  # 基线 = AI 动手前(确定性生成之后)的状态: 路径闸门只看 AI 改了什么(和 scripts/loop.sh 一样)。
  # 没有基线时,确定性生成的文件也算成 AI 的改动 → 闸门误判(CI 第一次真跑时发现)
  pre=$(git rev-parse HEAD)
  git add -A
  git -c user.name=factory-bot -c user.email=factory-bot@localhost commit -q --allow-empty -m "factory: baseline before AI"
  for s in "${ai_specs[@]}"; do
    name=$(basename "$(dirname "$s")")
    stage "④ AI 实现 $name" bash -c "go run ./factory/cmd/factory prompt '$s' | go run ./factory/cmd/factory ai --role implement ${IMPLEMENT_MODEL:+--model $IMPLEMENT_MODEL} -- --output-format json > .factory/impl-$name.json"
    echo "- \`$name\`: $(scripts/ci/ai-usage.sh ".factory/impl-$name.json")" >> .factory/ai-usage.md
  done
  stage "⑤ 路径 / 纯函数闸门" make --no-print-directory guard
  git reset -q --soft "$pre"   # 基线只给闸门用: 最后仍是一个生成物的提交
fi
stage "编译" make --no-print-directory build
sha=$(git rev-parse --short HEAD)
branch=factory/gen-$sha
git checkout -q -b "$branch"
git add -A
if git diff --cached --quiet; then
  echo "生成物没有变化"; md "" "生成物没有变化(不需要 PR)"; echo "$branch" > .factory/gen-branch; exit 0
fi
git -c user.name=factory-bot -c user.email=factory-bot@localhost commit -q -m "factory: 生成 $sha 的式样书"
echo "$branch" > .factory/gen-branch
scripts/ci/pr-body.sh "$BASE" "$specs" > .factory/pr-body.md
if [[ -s .factory/ai-usage.md ]]; then printf '\n### AI 的用量\n\n' >> .factory/pr-body.md; cat .factory/ai-usage.md >> .factory/pr-body.md; fi
echo "✓ 分支 $branch(PR 说明 .factory/pr-body.md)"
