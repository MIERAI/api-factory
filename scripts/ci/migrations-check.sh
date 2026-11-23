#!/usr/bin/env bash
# Versioned migrations (db/migrations, goose):
#   1. names NNNNN_<name>.sql, versions strictly increasing; every file has "-- +goose Up" and "-- +goose Down"
#   2. a migration that is on the base branch is never edited or deleted (it may already run in prod: fix forward)
#   3. drift: all migrations on an empty database == db/schema/*.sql on another (pg_dump --schema-only): the factory
#      reads db/schema, production runs the migrations; a change must be made in both
#   BASE (default origin/main): the branch to compare with. Needs docker.
set -euo pipefail
cd "$(dirname "$0")/../.."
BASE=${BASE:-origin/main}
fail=0
prev=0
for f in db/migrations/*.sql; do
  n=$(basename "$f")
  [[ $n =~ ^([0-9]{5})_[a-z0-9_]+\.sql$ ]] || { echo "✗ $n: name must be NNNNN_snake_case.sql"; fail=1; continue; }
  v=$((10#${BASH_REMATCH[1]}))
  (( v > prev )) || { echo "✗ $n: version $v not greater than $prev"; fail=1; }
  prev=$v
  grep -q '^-- +goose Up' "$f" || { echo "✗ $n: no '-- +goose Up'"; fail=1; }
  grep -q '^-- +goose Down' "$f" || { echo "✗ $n: no '-- +goose Down' (write the rollback, or an explicit no-op with a reason)"; fail=1; }
done
if git rev-parse -q --verify "$BASE" >/dev/null; then
  while read -r st path; do
    case $st in A) ;; *) echo "✗ $path: already on $BASE ($st): migrations are append-only, add a new one"; fail=1 ;; esac
  done < <(git -c core.quotePath=false diff --name-status "$BASE" -- db/migrations)
fi
(( fail == 0 )) || exit 1
echo "✓ migrations: $(ls db/migrations/*.sql | wc -l | tr -d ' ') files, versions increasing, Up + Down, append-only"

name=factory-drift-$$
docker run -d --rm --name "$name" -e POSTGRES_PASSWORD=t -e POSTGRES_USER=factory -e POSTGRES_DB=factory "${IMAGE_MIRROR:-mirror.gcr.io/library}/postgres:18" >/dev/null
trap 'docker rm -f "$name" >/dev/null' EXIT
until docker exec "$name" pg_isready -q -U factory 2>/dev/null; do sleep 1; done; sleep 1
psql() { docker exec -i -e PGOPTIONS="-c client_min_messages=warning" "$name" psql -q -U factory -v ON_ERROR_STOP=1 "$@"; }
psql -d factory -c "CREATE DATABASE from_migrations" -c "CREATE DATABASE from_schema"
cat db/schema/*.sql | psql -d from_schema >/dev/null
# migrations through the real tool (same code as production), inside the container's network namespace
GOOS=linux GOARCH=$(docker exec "$name" uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') CGO_ENABLED=0 go build -o .factory/migrate-linux ./cmd/migrate
docker cp .factory/migrate-linux "$name":/tmp/migrate >/dev/null
docker exec -e PGHOST=localhost -e PGUSER=factory -e PGPASSWORD=t -e MIGRATE_DATABASES=from_migrations "$name" /tmp/migrate up >/dev/null
dump() { docker exec "$name" pg_dump -U factory --schema-only --no-owner --no-privileges -T goose_db_version -T goose_db_version_id_seq "$1" | grep -vE '^(--|SET |SELECT pg_catalog|\\(un)?restrict )' | sed '/^$/d'; }
if diff <(dump from_schema) <(dump from_migrations) > .factory/schema-drift.diff; then
  echo "✓ schema drift: db/migrations == db/schema"
else
  echo "✗ schema drift: db/migrations and db/schema differ (< db/schema, > migrations):"; head -40 .factory/schema-drift.diff; exit 1
fi
