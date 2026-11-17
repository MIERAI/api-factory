#!/usr/bin/env bash
# 种类组合矩阵(P3): 在临时的 git 工作区里生成矩阵的设计稿,跑整条流水线(设计 → 生成 → 编译 → 验收含差分)。
#   usage: scripts/matrix.sh [--keep]     失败的地方就是没被试过的组合留下的通用 bug
set -uo pipefail
root=$(git rev-parse --show-toplevel)
wt=${MATRIX_DIR:-${TMPDIR:-/tmp}/api-factory-matrix}
git -C "$root" worktree remove --force "$wt" >/dev/null 2>&1; rm -rf "$wt"
git -C "$root" worktree add --detach "$wt" HEAD >/dev/null
cd "$wt"
python3 scripts/matrix.py . || exit 1
step() { echo "━━ $1"; shift; if ! "$@" > .factory-matrix.log 2>&1; then tail -30 .factory-matrix.log; echo "✗ $1 失败(工作区: $wt)"; exit 1; fi; }
step "db-schema" make db-schema
step "design" make design
step "prepare" make prepare
step "build" go build ./...
echo "━━ accept"
make accept ACCEPT_SPECS=specs/matrix 2>&1 | grep -E "^  ✗|acceptance" | head -60
rc=${PIPESTATUS[0]}
[[ "${1:-}" == "--keep" ]] || { cd "$root"; git worktree remove --force "$wt"; }
exit $rc
