#!/usr/bin/env bash
set -euo pipefail

# Build an isolated demo from this checkout. Existing demo deployments are untouched.
readonly repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly label="${ROOST_PERF_LABEL:-business-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || { echo 'Invalid ROOST_PERF_LABEL' >&2; exit 2; }
readonly output_dir="${repo_dir}/artifacts/perf/sync/${label}"
[[ ! -e "$output_dir" ]] || { echo "Output already exists: $output_dir" >&2; exit 2; }
readonly repeats="${ROOST_PERF_COUNT:-3}"
[[ "$repeats" =~ ^[1-9][0-9]*$ ]] || { echo 'ROOST_PERF_COUNT must be positive' >&2; exit 2; }
mkdir -p "$output_dir"
readonly scratch="$(mktemp -d "${TMPDIR:-/tmp}/roost-sync-business.XXXXXX")"
# Retain the generated project for pprof/reproduction, including on a failed gate.
printf '%s\n' "$scratch" > "$output_dir/workspace.txt"
export GOWORK=off
export GOMAXPROCS="${ROOST_PERF_CPU:-4}"
[[ "$GOMAXPROCS" =~ ^[1-9][0-9]*$ ]] || { echo 'ROOST_PERF_CPU must be positive' >&2; exit 2; }
{
 go version
 go env GOOS GOARCH GOWORK
 git -C "$repo_dir" rev-parse HEAD
 git -C "$repo_dir" status --short
 printf 'GOMAXPROCS=%s repeats=%s ticks=%s hz=%s case=%s\n' "$GOMAXPROCS" "$repeats" "${ROOST_SYNC_TICKS:-200}" "${ROOST_SYNC_HZ:-20}" "${ROOST_SYNC_CASE:-all}"
 printf 'custom_players=%s custom_dirty_percent=%s custom_layout=%s\n' "${ROOST_SYNC_PLAYERS:-64}" "${ROOST_SYNC_DIRTY_PERCENT:-10}" "${ROOST_SYNC_LAYOUT:-hotspot}"
} > "$output_dir/env.txt"
(cd "$repo_dir" && go build -o "$scratch/roost" ./codegen/cmd/roost)
"$scratch/roost" project new syncperf -module example.com/syncperf -out "$scratch/project" \
 -mods configdata,mongo,nats,dataengine,nest -services game -template game-demo -skip-deps \
 > "$output_dir/generate.log" 2>&1
cp "$repo_dir/scripts/perf/fixtures/sync_business_test.go.tmpl" "$scratch/project/internal/service/game/sync_business_test.go"
cd "$scratch/project"
go mod edit "-replace=github.com/tjbdwanghaibo/roost-core=$repo_dir"
go mod tidy > "$output_dir/dependencies.log" 2>&1
go test -c -o "$scratch/business.test" ./internal/service/game
export ROOST_SYNC_BUSINESS=1
result=0
for ((sample=1; sample<=repeats; sample++)); do
 export ROOST_SYNC_REPORT_DIR="$output_dir/sample-$sample"
 "$scratch/business.test" -test.run '^TestSyncBusinessLoad$' -test.v -test.timeout=20m \
  > "$output_dir/sample-$sample.log" 2>&1 || result=1
done
printf 'Results: %s\nGenerated demo: %s\n' "$output_dir" "$scratch/project"
exit "$result"
