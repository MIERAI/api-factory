#!/usr/bin/env bash
# 内容防护: 产品分支(非 trial-*)里不能出现试用公司的内容。
# 禁止词清单在仓库外(内容本身也不能进仓库): ${FACTORY_FORBIDDEN:-~/.config/api-factory/forbidden.txt}
#   用法: content-guard.sh --staged | --tree | --range <a..b>
set -uo pipefail
mode=${1:---tree}
branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "")
if [[ "$branch" == trial* ]]; then exit 0; fi   # 试用分支: 不检查
list=${FACTORY_FORBIDDEN:-$HOME/.config/api-factory/forbidden.txt}
if [[ ! -f "$list" ]]; then
  echo "⚠ content-guard: 没有禁止词清单 ($list),跳过"; exit 0
fi
pattern=$(grep -vE '^\s*(#|$)' "$list" | paste -sd'|' -)
[[ -z "$pattern" ]] && exit 0
case "$mode" in
  --staged) hits=$( { git diff --cached --name-only; git diff --cached -U0 | grep '^+' ; } | grep -inE "$pattern" | head -20) ;;
  --tree)   hits=$( { git ls-files; git grep -I -n -i -E "$pattern" -- . ':!scripts/content-guard.sh' 2>/dev/null; } | grep -inE "$pattern" | head -20) ;;
  --range)  hits=$( { git log --name-only --format= "$2"; git log -p --format= "$2" | grep '^+'; } | grep -inE "$pattern" | head -20) ;;
  *) echo "usage: content-guard.sh --staged | --tree | --range a..b"; exit 2 ;;
esac
if [[ -n "$hits" ]]; then
  echo "✗ content-guard: 产品分支($branch)里出现了禁止的内容(清单: $list):"
  echo "$hits" | sed 's/^/    /'
  echo "  试用的内容只能提交到 trial-* 分支。"
  exit 1
fi
