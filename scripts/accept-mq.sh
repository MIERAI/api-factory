#!/usr/bin/env bash
# 真实 RabbitMQ 的场景(docs/design/r4-events-messages.md §7.2 的第二层): 服务器带 AMQP_URL 运行(拓扑 / outbox 的中继 /
# 消费循环),验收执行器把消息发到交换机、看去重表 / DLQ / 重试队列 / 旁听队列(factory/accept/mq.go)。
# 共用一个 RabbitMQ 的 worker 会互相抢消息: 一个独立的 vhost + 一个独立的数据库,串行。make mq-up 先起 RabbitMQ。
#   scripts/accept-mq.sh [specs…]      (默认: specs)        PROJECT=examples/storefront-v2/inventory: 别的项目
set -euo pipefail
cd "$(dirname "$0")/.."
PG_NAME=${PG_NAME:-api-factory-pg}; MQ_NAME=${MQ_NAME:-api-factory-mq}; MQ_PORT=${MQ_PORT:-55672}
DB_URL=${DB_URL:-postgres://factory:factory@127.0.0.1:55432/factory?sslmode=disable}
id=mq$$
db="acc_${id}"; vhost="acc_${id}"
url="${DB_URL%/factory\?*}/${db}?sslmode=disable"
cleanup() {
  docker exec "$MQ_NAME" rabbitmqctl -q delete_vhost "$vhost" >/dev/null 2>&1 || true
  docker exec "$PG_NAME" psql -q -U factory -d factory -c "DROP DATABASE IF EXISTS $db" >/dev/null 2>&1 || true
}
trap cleanup EXIT
docker exec "$MQ_NAME" rabbitmqctl -q add_vhost "$vhost" >/dev/null
docker exec "$MQ_NAME" rabbitmqctl -q set_permissions -p "$vhost" guest '.*' '.*' '.*' >/dev/null
docker exec "$PG_NAME" psql -q -U factory -d factory -c "CREATE DATABASE $db" >/dev/null
P=${PROJECT:-.}
cat "$P"/db/schema/*.sql $(ls "$P"/db/views/*.sql 2>/dev/null) | docker exec -i -e PGOPTIONS="-c client_min_messages=warning" "$PG_NAME" psql -q -U factory -d "$db" -v ON_ERROR_STOP=1
go build -o .factory/factory ./factory/cmd/factory
F="$PWD/.factory/factory --project $(cd "$P" && pwd)"
(cd "$P" && go build -o bin/server ./cmd/server)
export AMQP_URL="amqp://guest:guest@127.0.0.1:${MQ_PORT}/${vhost}" FACTORY_TEST_AMQP="amqp://guest:guest@127.0.0.1:${MQ_PORT}/${vhost}"
$F serve-test --server bin/server --db "$url" -- $F accept --db "$url" "${@:-specs}" | grep -E 'RabbitMQ|✗|acceptance'
