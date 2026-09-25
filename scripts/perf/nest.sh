#!/usr/bin/env bash
set -euo pipefail

# 本地调度微基准；不依赖网络、数据库或示例游戏。
readonly repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly output_dir="${ROOST_PERF_OUTPUT:-${repo_dir}/artifacts/perf/nest}"
readonly label="${ROOST_PERF_LABEL:-$(date +%Y%m%d-%H%M%S)}"
readonly count="${ROOST_PERF_COUNT:-5}"
readonly benchtime="${ROOST_PERF_BENCHTIME:-1s}"
readonly bench="${ROOST_PERF_BENCH:-Benchmark(SlowDispatchWatch|TickCallbackSnapshot|ClientRequestSingle|ClientRequestMulti|ClientRequestStageMetrics)$}"
export GOWORK=off
export GOMAXPROCS="${ROOST_PERF_CPU:-4}"

if [[ ! "${label}" =~ ^[a-zA-Z0-9_-]+$ ]]; then
  printf 'ROOST_PERF_LABEL must contain only letters, digits, underscores or hyphens\n' >&2
  exit 2
fi
mkdir -p "${output_dir}"
cd "${repo_dir}"
{
  go version
  go env GOOS GOARCH GOWORK
  git rev-parse HEAD
  git status --short
  printf 'count=%s benchtime=%s GOMAXPROCS=%s bench=%s\n' "${count}" "${benchtime}" "${GOMAXPROCS}" "${bench}"
} > "${output_dir}/${label}.env.txt"

go test ./nest -run '^$' -bench "${bench}" -benchmem \
  -benchtime="${benchtime}" -count="${count}" -timeout=10m \
  | tee "${output_dir}/${label}.txt"

if [[ "${ROOST_PERF_PROFILE:-0}" == 1 ]]; then
  go test ./nest -run '^$' -bench '^BenchmarkClientRequestSingle$' -benchtime=2s \
    -cpuprofile="${output_dir}/${label}.cpu" -memprofile="${output_dir}/${label}.mem" \
    -o "${output_dir}/${label}.test" > "${output_dir}/${label}-profile.txt"
  go tool pprof -top -nodecount=15 "${output_dir}/${label}.test" "${output_dir}/${label}.cpu" \
    > "${output_dir}/${label}-cpu-top.txt"
  go tool pprof -top -alloc_space -nodecount=15 "${output_dir}/${label}.test" "${output_dir}/${label}.mem" \
    > "${output_dir}/${label}-alloc-top.txt"
fi
printf 'Benchmark output: %s/%s.txt\n' "${output_dir}" "${label}"
