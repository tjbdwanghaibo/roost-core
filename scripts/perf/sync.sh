#!/usr/bin/env bash
set -euo pipefail

# CPU/memory microbenchmarks only; no live broker or network clients required.
readonly repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly output_dir="${ROOST_PERF_OUTPUT:-${repo_dir}/artifacts/perf/sync}"
readonly label="${ROOST_PERF_LABEL:-$(date +%Y%m%d-%H%M%S)}"
readonly count="${ROOST_PERF_COUNT:-5}"
readonly benchtime="${ROOST_PERF_BENCHTIME:-1s}"
readonly cpu="${ROOST_PERF_CPU:-1}"
readonly bench="${ROOST_PERF_BENCH:-.}"

# label is a filename, never a path or command.
if [[ ! "${label}" =~ ^[a-zA-Z0-9_-]+$ ]]; then
  printf 'ROOST_PERF_LABEL must contain only letters, digits, underscores or hyphens\n' >&2
  exit 2
fi
mkdir -p "${output_dir}"
cd "${repo_dir}"
{
  go version
  go env GOOS GOARCH GOMAXPROCS GOWORK
  git rev-parse HEAD
  git status --short
  printf 'count=%s benchtime=%s cpu=%s bench=%s\n' "${count}" "${benchtime}" "${cpu}" "${bench}"
} > "${output_dir}/${label}.env.txt"

go test ./sync/... -run '^$' -bench "${bench}" -benchmem \
  -benchtime="${benchtime}" -count="${count}" -cpu="${cpu}" -timeout=20m \
  | tee "${output_dir}/${label}.txt"
printf 'Benchmark output: %s/%s.txt\n' "${output_dir}" "${label}"
