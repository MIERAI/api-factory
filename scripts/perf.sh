#!/usr/bin/env bash
# Performance gate (make perf / CI): every API of this repository and of every project under examples/ (factory
# --project) against ~200k rows per table, on a fresh Postgres container of its own (removed at the end; nothing shared
# with make db-up or another run). factory perf: data generator → EXPLAIN ANALYZE → plan rules (factory/perf).
#   scripts/perf.sh [dir…]     dirs: "." = this repository, examples/… = a project (default: all)
#   PERF_ARGS="--rows 50000"   passed to factory perf
# Serial on purpose: the time threshold is calibrated on this database (factory/perf Calibrate); parallel runs on the
# same machine would measure each other.
set -euo pipefail
cd "$(dirname "$0")/.."
IMAGE=${PG_IMAGE:-${IMAGE_MIRROR:-mirror.gcr.io/library}/postgres:16-alpine}
name=factory-perf-$$
docker run -d --rm --name "$name" -e POSTGRES_USER=factory -e POSTGRES_PASSWORD=factory -e POSTGRES_DB=factory \
  -e POSTGRES_INITDB_ARGS="--locale-provider=icu --icu-locale=en-US" -p 127.0.0.1::5432 "$IMAGE" >/dev/null
trap 'docker rm -f -v "$name" >/dev/null 2>&1 || true' EXIT
until docker exec "$name" pg_isready -q -U factory -d factory 2>/dev/null && docker exec "$name" psql -q -U factory -d factory -c 'select 1' >/dev/null 2>&1; do sleep 0.5; done
port=$(docker port "$name" 5432/tcp | head -1 | sed 's/.*://')
go build -o .factory/factory ./factory/cmd/factory
F=$PWD/.factory/factory

dirs=("$@")
if [ ${#dirs[@]} -eq 0 ]; then
  dirs=(.)
  while IFS= read -r d; do dirs+=("$d"); done < <(scripts/projects.sh --list)
fi
fail=0
mkdir -p .factory
for d in "${dirs[@]}"; do
  db=perf_$(echo "$d" | tr -c 'a-zA-Z0-9\n' '_' | tr 'A-Z' 'a-z')
  echo "━━ perf $d"
  docker exec "$name" psql -q -U factory -d factory -c "CREATE DATABASE $db" >/dev/null
  # shellcheck disable=SC2046
  cat "$d"/db/schema/*.sql $(ls "$d"/db/views/*.sql 2>/dev/null) |
    docker exec -i -e PGOPTIONS="-c client_min_messages=warning" "$name" psql -q -U factory -d "$db" -v ON_ERROR_STOP=1
  out=.factory/perf-$(echo "$d" | tr '/.' '__').txt
  (
    set -e
    "$F" --project "$d" prepare >/dev/null
    # shellcheck disable=SC2086
    "$F" --project "$d" perf --db "postgres://factory:factory@127.0.0.1:${port}/${db}?sslmode=disable" ${PERF_ARGS:-}
  ) > "$out" 2>&1 || { fail=1; }
  # everything but ✓, with the detail lines
  awk '/^[^ ]/ { show = ($0 !~ /^✓  /) } show' "$out"
done
exit $fail
