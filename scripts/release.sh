#!/usr/bin/env bash
# One product, one version: factory vX.Y.Z and the runtime module (runtime/) are released together as two tags on the
# same commit, vX.Y.Z and runtime/vX.Y.Z (factory/version/version.go).
#
#   scripts/release.sh check            every place that spells the version agrees (CI: runtime.yml versions)
#   scripts/release.sh bump vX.Y.Z      change them all: factory/version, runtime/version, go.mod, examples/ (factory
#                                       upgrade: go.mod + tidy + build); then commit through a PR as usual
#   scripts/release.sh tag [--push]     after that PR is merged: on main's HEAD, create the annotated tags vX.Y.Z and
#                                       runtime/vX.Y.Z (--push: push both). A tag is never moved: the Go module proxy
#                                       and checksum database remember runtime/vX.Y.Z forever.
set -euo pipefail
cd "$(dirname "$0")/.."
RT=github.com/MIERAI/api-factory/runtime
factory_version() { sed -n 's/^const Factory = "\(.*\)"/\1/p' factory/version/version.go; }
runtime_version() { sed -n 's/^const Version = "\(.*\)"/\1/p' runtime/version/version.go; }
projects() { find examples -name go.mod -not -path '*/.factory/*' | sort | while IFS= read -r m; do dirname "$m"; done; }

check() {
  local v rv fail=0
  v=$(factory_version); rv=$(runtime_version)
  [[ $v == "$rv" ]] || { echo "✗ factory/version $v ≠ runtime/version $rv"; fail=1; }
  for m in go.mod $(projects | sed 's#$#/go.mod#'); do
    have=$(awk -v rt="$RT" '$1 == rt {print $2} $1 == "require" && $2 == rt {print $3}' "$m")
    [[ $have == "$v" ]] || { echo "✗ $m: $RT ${have:-missing}, the factory is $v"; fail=1; }
  done
  (( fail == 0 )) && echo "✓ factory $v = runtime $rv = every go.mod"
  return $fail
}

case ${1:-} in
  check) check ;;
  bump)
    new=${2:?usage: scripts/release.sh bump vX.Y.Z}
    [[ $new =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "✗ $new: vX.Y.Z"; exit 1; }
    sed -i.bak "s/^const Factory = \".*\"/const Factory = \"$new\"/" factory/version/version.go
    sed -i.bak "s/^const Version = \".*\"/const Version = \"$new\"/" runtime/version/version.go
    rm -f factory/version/version.go.bak runtime/version/version.go.bak
    go mod edit -require="$RT@$new"
    go build -o .factory/factory ./factory/cmd/factory
    for d in $(projects); do .factory/factory upgrade --project "$d"; done
    check ;;
  tag)
    v=$(factory_version)
    check >/dev/null || { check; exit 1; }
    [[ -z $(git status --porcelain) ]] || { echo "✗ the working tree is not clean"; exit 1; }
    git fetch -q origin main --tags
    [[ $(git rev-parse HEAD) == "$(git rev-parse origin/main)" ]] || { echo "✗ HEAD is not origin/main: tag the merged commit"; exit 1; }
    for t in "$v" "runtime/$v"; do
      git rev-parse -q --verify "refs/tags/$t" >/dev/null && { echo "✗ tag $t exists (tags are never moved)"; exit 1; }
    done
    git tag -a "$v" -m "api-factory $v (runtime/$v)"
    git tag -a "runtime/$v" -m "api-factory runtime $v ($RT, paired with factory $v)"
    echo "✓ tags $v and runtime/$v on $(git rev-parse --short HEAD)"
    if [[ ${2:-} == --push ]]; then git push origin "refs/tags/$v" "refs/tags/runtime/$v"; fi ;;
  *) sed -n '2,12p' "$0"; exit 2 ;;
esac
