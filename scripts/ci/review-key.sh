#!/usr/bin/env bash
# 审查的输入的指纹: 编译后的式样书 + 设计书 / 审查记录 / 领域(和领域的审查记录)+ 审查的 prompt 模板 + 模型。
# 指纹相同 = 审查员看到的东西相同 → 上次通过的结果可以沿用(同一个设计书前后审查结论翻转的问题: 不重审)。
#   scripts/ci/review-key.sh <spec.yaml> [model]
set -euo pipefail
spec=$1; model=${2:-default}
design=$(sed -n '1s/.* from \(design\/[^ ]*\.yaml\)\..*/\1/p' "$spec")
files=("$spec" factory/prompts/review.md)
if [[ -n $design ]]; then
  dir=$(dirname "$design")
  for f in "$design" "${design%.yaml}.审查.yaml" "$dir/领域.yaml" "$dir/领域.审查.yaml"; do
    [[ -f $f ]] && files+=("$f")
  done
fi
sum() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi; }
{ for f in "${files[@]}"; do printf '== %s\n' "$f"; cat "$f"; done; printf '== model %s\n' "$model"; } | sum | cut -c1-16
