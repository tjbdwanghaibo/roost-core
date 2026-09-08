#!/usr/bin/env bash
set -euo pipefail

# Local benchmark run for the data engine path: WAL codec / writer (nestwal),
# projector + Mongo adapter (dataengine/engine) and Saga reservation overhead
# (saga). Moved from roost-kit/scripts/perf when the implementations moved to
# core (v1.14.0); package paths are the only change.
#
# For an A/B against another checkout, set ROOST_PERF_COUNT=10 and run this
# script in each tree with GOWORK=off, then normalise the `pkg:` lines and
# compare with `benchstat` (see docs/history/P5_acceptance.md §4).

readonly repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly go_cache="${ROOST_GO_CACHE:-$(cd /tmp && pwd -P)/roost-go-cache}"
readonly output_dir="${ROOST_PERF_OUTPUT:-${repo_dir}/artifacts/perf}"
readonly count="${ROOST_PERF_COUNT:-5}"

mkdir -p "${output_dir}"
cd "${repo_dir}"

GOCACHE="${go_cache}" go test ./nestwal ./dataengine/engine ./saga \
  -run '^$' -bench . -benchmem -count="${count}" -timeout 0 \
  | tee "${output_dir}/dataengine-local.txt"

cat <<'MSG'
Local benchmark complete. This output covers codec, WAL, projector adapter,
Mongo adapter, and Saga reservation overhead only. Production release gates
must also run the replica-set + JetStream file-storage profile, including
primary/leader failover and a 100k-record recovery backlog.
MSG
