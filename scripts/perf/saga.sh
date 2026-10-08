#!/usr/bin/env bash
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
: "${ROOST_DATAENGINE_IT_ROOT:?source private environment env.sh}"
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
label="${ROOST_PERF_LABEL:-saga-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || exit 2
output="$repo/artifacts/perf/saga/$label"
[[ ! -e "$output" ]] || exit 2
lock="$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock"
mkdir "$lock" || { echo 'Another acceptance run owns the private environment' >&2; exit 2; }
trap 'rmdir "$lock"' EXIT
mkdir -p "$output"
cd "$repo"
export GOWORK=off GOMAXPROCS="${ROOST_PERF_CPU:-4}"
modes=(forward compensate)
if [[ -n "${ROOST_SAGA_PERF_MODE:-}" ]]; then
 case "$ROOST_SAGA_PERF_MODE" in
  forward|compensate) modes=("$ROOST_SAGA_PERF_MODE") ;;
  *) echo 'Mode must be forward or compensate' >&2; exit 2 ;;
 esac
fi
{ go version; git rev-parse HEAD; git status --short; shasum -a 256 saga/*.go; env | sort | sed -n '/^ROOST_SAGA_PERF_/p'; } > "$output/env.txt"
status=0
for mode in "${modes[@]}";do
 ROOST_SAGA_PERF_OUTPUT="$output/$mode.json" ROOST_SAGA_PERF_MODE="$mode" go test -tags=integration ./saga -run '^TestRealSagaPerformance$' -count=1 -timeout=15m -v > "$output/$mode.log" 2>&1 || status=1
 tail -n 8 "$output/$mode.log"
done
printf 'Results: %s\n' "$output"
exit "$status"
