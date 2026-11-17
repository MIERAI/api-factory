#!/bin/sh
# 建表(幂等): 主库 factory + 分库 shard_a / shard_b。连接信息: PGHOST / PGUSER / PGPASSWORD(Kubernetes 的 Job 给)
set -eu
export PGOPTIONS="-c client_min_messages=warning"
until pg_isready -q; do echo "waiting for postgres…"; sleep 2; done
for db in shard_a shard_b; do
  psql -d factory -tAc "SELECT 1 FROM pg_database WHERE datname='$db'" | grep -q 1 || psql -d factory -c "CREATE DATABASE $db"
done
for db in factory shard_a shard_b; do
  for f in /schema/*.sql; do psql -q -d "$db" -v ON_ERROR_STOP=1 -f "$f"; done
  echo "✓ $db"
done
