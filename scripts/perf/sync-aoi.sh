#!/usr/bin/env bash
set -euo pipefail
readonly repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly label="${ROOST_PERF_LABEL:-aoi-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || { echo 'Invalid label' >&2; exit 2; }
readonly output="$repo_dir/artifacts/perf/sync/$label"
[[ ! -e "$output" ]] || { echo "Output already exists: $output" >&2; exit 2; }
readonly repeats="${ROOST_PERF_COUNT:-3}"
[[ "$repeats" =~ ^[1-9][0-9]*$ ]] || { echo 'Invalid repeat count' >&2; exit 2; }
export GOMAXPROCS="${ROOST_PERF_CPU:-4}"
export GOWORK=off
[[ "$GOMAXPROCS" =~ ^[1-9][0-9]*$ ]] || { echo 'Invalid CPU count' >&2; exit 2; }
mkdir -p "$output"
cd "$repo_dir"
{
 go version
 git rev-parse HEAD
 git status --short
 printf 'GOMAXPROCS per process=%s\n' "$GOMAXPROCS"
 printf 'args:'; printf ' %q' "$@"; printf '\n'
} > "$output/env.txt"
go build -o "$output/sync-aoi" ./scripts/perf/sync-aoi
status=0
for ((sample=1; sample<=repeats; sample++)); do
 "$output/sync-aoi" "$@" -output="$output/sample-$sample" > "$output/sample-$sample.log" 2>&1 || status=1
 cat "$output/sample-$sample.log"
done
printf 'Results: %s\n' "$output"
exit "$status"
