#!/usr/bin/env bash
set -euo pipefail

# 仅测单 ID 调度；CPU/profile/race 不与正式吞吐样本混跑。
readonly repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly label="${ROOST_PERF_LABEL:-dispatch-$(date +%Y%m%d-%H%M%S)}"
readonly count="${ROOST_PERF_COUNT:-3}"
readonly benchtime="${ROOST_PERF_BENCHTIME:-2s}"
readonly bench="${ROOST_PERF_BENCH:-^BenchmarkDispatchCompare(Producers)?$}"
export GOWORK=off
export GOMAXPROCS="${ROOST_PERF_CPU:-4}"
if [[ ! "${label}" =~ ^[a-zA-Z0-9_-]+$ ]]; then
  printf 'ROOST_PERF_LABEL must contain only letters, digits, underscores or hyphens\n' >&2
  exit 2
fi
cd "${repo_dir}"
readonly output_dir="${repo_dir}/artifacts/perf/nest/${label}"
# 不覆盖同标签的失败或既有样本。
mkdir -p "${repo_dir}/artifacts/perf/nest"
mkdir "${output_dir}"
{
  go version
  go env GOOS GOARCH GOWORK
  uname -a
  if [[ "$(uname -s)" == Darwin ]]; then
    sysctl -n machdep.cpu.brand_string
  elif [[ -r /proc/cpuinfo ]]; then
    awk -F ': ' '/model name/ {print $2; exit}' /proc/cpuinfo
  fi
  git rev-parse HEAD
  git status --short
  git hash-object framework/nest/dispatch_queue.go framework/nest/dispatch_compare_bench_test.go
  printf 'count=%s benchtime=%s workers=GOMAXPROCS=%s window=64 queue=65536 bench=%s\n' "${count}" "${benchtime}" "${GOMAXPROCS}" "${bench}"
} > "${output_dir}/env.txt"
go test -c -o "${output_dir}/nest.test" ./framework/nest
"${output_dir}/nest.test" -test.run '^TestDispatchComparisonSingleIDContract$' \
  -test.count=3 -test.timeout=1m | tee "${output_dir}/contract.txt"
"${output_dir}/nest.test" -test.run '^$' -test.bench "${bench}" -test.benchmem \
  -test.benchtime="${benchtime}" -test.count="${count}" -test.cpu="${GOMAXPROCS}" \
  -test.timeout="${ROOST_PERF_TIMEOUT:-30m}" | tee "${output_dir}/results.txt"

if [[ "${ROOST_PERF_PROFILE:-0}" == 1 ]]; then
  # 单独诊断，两种实现各存一组 profile；这些输出不可并入上面的吞吐样本。
  for kind in nest id; do
    "${output_dir}/nest.test" -test.run '^$' \
      -test.bench "^BenchmarkDispatchCompare$/noop/uniform/${kind}$" \
      -test.benchtime=5s -test.cpu="${GOMAXPROCS}" \
      -test.cpuprofile="${output_dir}/${kind}.cpu" \
      -test.mutexprofile="${output_dir}/${kind}.mutex" -test.mutexprofilefraction=1 \
      -test.blockprofile="${output_dir}/${kind}.block" -test.blockprofilerate=1 \
      > "${output_dir}/${kind}-profile.txt"
  done
fi
printf 'Benchmark output: %s\n' "${output_dir}"
