#!/usr/bin/env bash
# 阶段 2: 式样书合并到 main 之后。生成代码(确定性;计算 / 自定义的 API 由 AI 写纯函数)→ 生成物的分支 + PR 的说明。
#   BASE: 合并前的提交(GitHub: github.event.before)。IMPLEMENT_MODEL: factory.yaml 的模型(空 = 角色的默认)。输出: 分支 factory/gen-<sha>,PR 说明 .factory/pr-body.md
#   GitHub 上由 workflow 推送分支、gh pr create;本地模拟由 sim.sh 合并
source "$(dirname "$0")/lib.sh"
BASE=${BASE:?BASE=合并前的提交}
md "## 生成" "" "| 步骤 | 时间 |" "|---|---|"
stage "② 确定性生成" bash -c "make --no-print-directory design >/dev/null && make --no-print-directory prepare"
specs=$(changed_specs "$BASE")
ai=0
for s in $specs; do
  if [[ $(go run ./factory/cmd/factory ai-paths "$s" | wc -l) -gt 1 ]]; then # AI 要写的文件(计算 / 自定义)
    ai=1
    stage "④ AI 实现 $(basename "$(dirname "$s")")" bash -c "go run ./factory/cmd/factory prompt '$s' | go run ./factory/cmd/factory ai --role implement ${IMPLEMENT_MODEL:+--model $IMPLEMENT_MODEL} -- --output-format text"
  fi
done
if [[ $ai == 1 ]]; then stage "⑤ 路径 / 纯函数闸门" make --no-print-directory guard; fi
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
echo "✓ 分支 $branch(PR 说明 .factory/pr-body.md)"
