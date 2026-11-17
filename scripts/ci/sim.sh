#!/usr/bin/env bash
# 本地模拟整条流水线(GitHub 用本地的 bare 仓库代替;各阶段跑的是 CI 里同样的脚本):
#   式样书的 PR → 合并 → 生成物的 PR(机器人)→ 验证 → 合并 → 部署(本地的 Cloud Run 模拟)→ 部署后的验收
#   usage: scripts/ci/sim.sh [design 文件](默认 scripts/ci/sim/GetDailySales.yaml → design/sales/)
#   REVIEW=skip(默认): 不调用 AI 审查;REVIEW=on: 调用(要 claude)
set -euo pipefail
ROOT=$(git rev-parse --show-toplevel)
NEW=${1:-$ROOT/scripts/ci/sim/GetDailySales.yaml}
DEST=${DEST:-design/sales/$(basename "$NEW")}
SIM=${SIM_DIR:-$ROOT/.factory/sim}
export REVIEW=${REVIEW:-skip}
rm -rf "$SIM"; mkdir -p "$SIM"
export CI_SUMMARY=$SIM/summary.md
: > "$CI_SUMMARY"
git clone -q --bare "$ROOT" "$SIM/origin.git"                 # "GitHub"
git clone -q "$SIM/origin.git" "$SIM/work"
cd "$SIM/work"
git config user.name sim; git config user.email sim@localhost
T0=$(date +%s)
lap() { echo; echo "■ $1 (累计 $(( $(date +%s) - T0 ))s)"; echo "| **$1** | 累计 $(( $(date +%s) - T0 ))s |" >> "$CI_SUMMARY"; }

# 1. 式样书的 PR
git checkout -q -b spec/$(basename "$NEW" .yaml)
cp "$NEW" "$DEST"; git add "$DEST"; git commit -q -m "式样书: $(basename "$NEW" .yaml)"
BASE=main scripts/ci/spec-review.sh
lap "式样书的 PR: 检查通过"

# 2. 合并(人 review 式样书之后)
before=$(git rev-parse main)
git checkout -q main; git merge -q --no-ff -m "Merge spec/$(basename "$NEW" .yaml)" spec/$(basename "$NEW" .yaml); git push -q origin main
M0=$(date +%s)

# 3. 生成物的 PR(机器人)
BASE=$before scripts/ci/generate.sh
branch=$(cat .factory/gen-branch)
git push -q origin "$branch"
cp .factory/pr-body.md "$SIM/pr-body.md"
lap "生成物的 PR: $branch"

# 4. 验证(CI)
scripts/ci/verify.sh
lap "生成物的 PR: 验证通过"

# 5. 合并(管理者 review 之后)
git checkout -q main; git merge -q --no-ff -m "Merge $branch" "$branch"; git push -q origin main

# 6. 部署
TARGET=local scripts/ci/deploy.sh
lap "部署完成"
echo
echo "✓ 式样书合并 → 新版本在服务: $(( $(date +%s) - M0 ))s(不含人 review 的时间)"
echo "  PR 说明: $SIM/pr-body.md   各阶段: $CI_SUMMARY"
echo "  服务中: $(cat .factory/sim-current)"
