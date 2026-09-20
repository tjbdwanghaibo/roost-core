#!/usr/bin/env bash
# Local mirror of the framework-compat "source-head" lane: generate a project
# with the roost CLI built from this checkout, then compile and test it against
# this WORKING TREE through a temporary go.work, and check the generated go.mod
# stayed publishable (no replace directives).
#
#   codegen/scripts/source-head-check.sh [minimal|full]
#
# One module since the consolidation, so there is no second working tree to
# point at and no framework version to pin: generation runs with --skip-deps
# (the imports it writes may only exist in this tree, not in any release) and
# the workspace supplies the framework. Set ROOST_KEEP=1 to keep the temporary
# directory for inspection.
set -euo pipefail

scenario="${1:-minimal}"
# codegen/scripts/… → the module root is two levels up.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

work="$(mktemp -d "${TMPDIR:-/tmp}/roost-source-head.XXXXXX")"
cleanup() { if [[ "${ROOST_KEEP:-0}" != 1 ]]; then rm -rf "$work"; else echo "kept: $work"; fi; }
trap cleanup EXIT

echo "source-head-check: building roost CLI from $repo_root"
(cd "$repo_root" && GOWORK=off go build -o "$work/roost" ./codegen/cmd/roost)

mods=configdata
services=game
template_args=()
if [[ "$scenario" == full ]]; then
  mods=configdata,etcd,redis,mongo,nats,sync,remote_entity,dataengine,nest,saga
  services=game,gate
  template_args=(-template game)
fi

echo "source-head-check: generating planet ($scenario) against this working tree"
GOWORK=off "$work/roost" project new planet -skip-deps -module example.com/planet -out "$work/planet" \
  -mods "$mods" -services "$services" ${template_args[@]+"${template_args[@]}"}
if [[ "$scenario" == full ]]; then
  (cd "$work/planet" && GOWORK=off "$work/roost" add access player --service gate \
    && GOWORK=off "$work/roost" add transport tcp --service gate \
    && GOWORK=off "$work/roost" add skill Fireball \
    && GOWORK=off "$work/roost" add saga GuildTransfer -service game -steps debit,credit) || true
fi

if grep -qE '^[[:space:]]*replace[[:space:](]' "$work/planet/go.mod"; then
  echo "source-head-check: generated go.mod carries a replace directive" >&2
  exit 1
fi

echo "source-head-check: compiling planet against the working tree $repo_root"
(cd "$work" && go work init ./planet "$repo_root" && go work edit -go=1.27.0)
export GOWORK="$work/go.work"
# --skip-deps skipped `go mod tidy`, and tidy is what upgrades the pre-split
# google.golang.org/genproto that etcd's old requirement drags in; left alone,
# it and the split googleapis/{api,rpc} modules provide the same package and
# every build fails with "ambiguous import". tidy cannot run (it ignores the
# workspace and the framework version may be unpublished), so do that one piece
# of its work explicitly.
(cd "$work/planet" && go mod edit -droprequire=github.com/tjbdwanghaibo/roost-core \
  && GOFLAGS=-mod=mod go get google.golang.org/genproto@latest >/dev/null)
(cd "$work/planet" && go build ./... && go vet ./... && go test -count=1 ./... 2>&1 | { grep -v "no test files" || true; })
(cd "$work/planet" && go run github.com/tjbdwanghaibo/roost-core/cmd/glsvet ./...)
echo "source-head-check: $scenario OK"
