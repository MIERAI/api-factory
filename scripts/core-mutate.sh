#!/usr/bin/env bash
# 元件认证的变异测试(清单和执行: scripts/core-mutate.py)。
# 用法: FACTORY_TEST_DB=postgres://... scripts/core-mutate.sh [--workers N] [--list] [名字的正则]
exec python3 "$(dirname "$0")/core-mutate.py" "$@"
