#!/usr/bin/env bash
# 环境检查: 工具、数据库、AI 命令行。make loop 之前先跑这个。
ok=0
check() { if eval "$2" >/dev/null 2>&1; then echo "  ✓ $1"; else echo "  ✗ $1 —— $3"; ok=1; fi; }
echo "工具:"
check "go"     "command -v go"     "brew install go"
check "buf"    "command -v buf"    "brew install bufbuild/buf/buf"
check "sqlc"   "command -v sqlc"   "brew install sqlc"
check "docker" "docker info"       "启动 docker / orbstack"
echo "数据库:"
check "Postgres 容器 ${PG_NAME:-api-factory-pg}" "docker exec ${PG_NAME:-api-factory-pg} psql -U factory -d factory -c 'select 1'" "make db-up"
check "分库 shard_a / shard_b" "docker exec ${PG_NAME:-api-factory-pg} psql -U factory -d shard_a -c 'select 1'" "make db-schema"
echo "AI:"
check "claude 在 PATH 里" "command -v claude" "后台 / 非交互的 shell 里 PATH 可能不同: PATH=/opt/homebrew/bin:\$PATH"
check "claude 能执行" "claude --version" "claude auth login"
echo "仓库:"
check "钩子已安装(core.hooksPath)" "[ \"\$(git config core.hooksPath)\" = scripts/hooks ]" "make hooks"
check "内容防护" "scripts/content-guard.sh --tree" "见上面的输出"
exit $ok
