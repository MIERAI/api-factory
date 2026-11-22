#!/usr/bin/env bash
# The factory's other projects (factory --project): every directory under examples/ with a factory.yaml — today the
# storefront v2 sample (docs/design/storefront-v2-readiness.md). For each one, the same gates as this repository:
#   design / check     the design compiles, the specs pass the gate
#   prepare            the generated code is up to date (regenerating changes nothing tracked)
#   build              go build + go vet in the project's own module
#   accept             a database of its own (sf_<name> on the factory's Postgres) with its db/schema, the server,
#                      hand-written + automatic + differential cases
# Then, per system (the projects' parent directory, e.g. examples/storefront-v2): factory system --check = the
# cross-project message contract check and the system topology manifest <contracts>/topology.json is up to date.
#   scripts/projects.sh [dir…]      (default: all)       PROJECTS_REGEN=1: write the regenerated files instead of failing
set -euo pipefail
cd "$(dirname "$0")/.."
PG_NAME=${PG_NAME:-api-factory-pg}
PORT=${FACTORY_DB_PORT:-55432}
go build -o .factory/factory ./factory/cmd/factory
F=$PWD/.factory/factory

dirs=("$@")
if [ ${#dirs[@]} -eq 0 ]; then
  # factory.yaml, not go.mod: hand-written modules next to the projects (examples/storefront-v2/e2e/driver) are not projects
  while IFS= read -r m; do dirs+=("$(dirname "$m")"); done < <(find examples -name factory.yaml -not -path '*/node_modules/*' | sort)
fi
[ ${#dirs[@]} -gt 0 ] || { echo "no projects under examples/"; exit 0; }

fail=0
for d in "${dirs[@]}"; do
  name=$(basename "$d"); db="sf_${name//-/_}"
  url="postgres://factory:factory@127.0.0.1:${PORT}/${db}?sslmode=disable"
  echo "━━ $d"
  (
    set -e
    "$F" --project "$d" design >/dev/null
    "$F" --project "$d" check >/dev/null
    "$F" --project "$d" prepare >/dev/null
    (cd "$d" && go mod tidy)
    # regenerating changed a tracked file, or produced a file git does not know (index = what is committed / staged)
    if [ -z "${PROJECTS_REGEN:-}" ] && { ! git diff --quiet -- "$d" || [ -n "$(git ls-files --others --exclude-standard -- "$d")" ]; }; then
      git diff --stat -- "$d"; git ls-files --others --exclude-standard -- "$d"; echo "✗ $d: generated files are not up to date (scripts/projects.sh with PROJECTS_REGEN=1, commit)"; exit 1
    fi
    (cd "$d" && go build ./... && go vet ./... && go build -o bin/server ./cmd/server)
    docker exec "$PG_NAME" psql -q -U factory -d factory -c "DROP DATABASE IF EXISTS $db" -c "CREATE DATABASE $db" 2>/dev/null
    cat "$d"/db/schema/*.sql | docker exec -i "$PG_NAME" psql -q -U factory -d "$db" -v ON_ERROR_STOP=1
    "$F" --project "$d" serve-test --server bin/server --db "$url" -- "$F" --project . accept --db "$url" | tail -1
  ) || { echo "✗ $d"; fail=1; }
done

# systems: the parent directories of the projects checked (PROJECTS_REGEN=1: rewrite topology.json)
systems=$(for d in "${dirs[@]}"; do dirname "$d"; done | sort -u)
for sys in $systems; do
  echo "━━ system $sys"
  if [ -n "${PROJECTS_REGEN:-}" ]; then "$F" system "$sys" || fail=1; else "$F" system "$sys" --check || fail=1; fi
done
exit $fail
