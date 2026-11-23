#!/usr/bin/env bash
# 审查的输入的指纹(沿用上次审查结果的键): API 的设计书 + 审查记录 + 领域里它引用的部分(表 / 列 / 规则 / 枚举 / 时区)
# + 领域的审查记录 + 审查的 prompt 模板 + 模型。领域里加了无关的表 → 指纹不变(factory/review/fingerprint.go)。
# 指纹相同 = 审查员判断这个 API 用到的东西相同 → 上次通过的结果可以沿用(同一个设计书前后审查结论翻转的问题: 不重审)。
#   scripts/ci/review-key.sh <spec.yaml> [model]      PROJECT=<项目目录>: 项目里的式样书
set -euo pipefail
exec go run ./factory/cmd/factory ${PROJECT:+--project "$PROJECT"} review-key --model "${2:-default}" "$1"
