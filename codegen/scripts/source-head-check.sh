#!/usr/bin/env bash
# Local mirror of the framework-compat "source-head" lane: generate a project
# with the roost CLI built from this checkout, then compile and test it against
# the roost-core / roost-kit WORKING TREES through a temporary go.work, and
# check the generated go.mod stayed publishable (no replace directives).
#
#   scripts/source-head-check.sh [minimal|full] [core-dir] [kit-dir]
#
# Bootstrap resolution inside `project new` runs with GOWORK=off against the
# module proxy; it pins the released consolidated layout by default, override
# with ROOST_CORE_PIN / ROOST_KIT_PIN (pre-releases work too). Set ROOST_KEEP=1 to
# keep the temporary directory for inspection.
set -euo pipefail

scenario="${1:-minimal}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
core_dir="$(cd "${2:-$repo_root/../roost-core}" && pwd)"
kit_dir="$(cd "${3:-$repo_root/../roost-kit}" && pwd)"
core_pin="${ROOST_CORE_PIN:-v1.15.18}"
kit_pin="${ROOST_KIT_PIN:-v1.14.17}"

work="$(mktemp -d "${TMPDIR:-/tmp}/roost-source-head.XXXXXX")"
cleanup() { if [[ "${ROOST_KEEP:-0}" != 1 ]]; then rm -rf "$work"; else echo "kept: $work"; fi; }
trap cleanup EXIT

echo "source-head-check: building roost CLI from $repo_root"
(cd "$repo_root" && GOWORK=off go build -o "$work/roost" ./cmd/roost)

mods=configdata
services=game
template_args=()
if [[ "$scenario" == full ]]; then
  mods=configdata,etcd,redis,mongo,nats,sync,remote_entity,dataengine,nest,saga
  services=game,gate
  template_args=(-template game)
fi

echo "source-head-check: generating planet ($scenario) pinned to core $core_pin / kit $kit_pin"
GOWORK=off "$work/roost" project new planet -module example.com/planet -out "$work/planet" \
  -mods "$mods" -services "$services" \
  -roost-core-version "$core_pin" -roost-kit-version "$kit_pin" -codegen-version latest ${template_args[@]+"${template_args[@]}"}
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

echo "source-head-check: compiling planet against working trees $core_dir and $kit_dir"
(cd "$work" && go work init ./planet "$core_dir" "$kit_dir" && go work edit -go=1.27.0)
export GOWORK="$work/go.work"
(cd "$work/planet" && go build ./... && go vet ./... && go test -count=1 ./... 2>&1 | { grep -v "no test files" || true; })
(cd "$work/planet" && go run github.com/tjbdwanghaibo/roost-core/cmd/glsvet ./...)
echo "source-head-check: $scenario OK"
