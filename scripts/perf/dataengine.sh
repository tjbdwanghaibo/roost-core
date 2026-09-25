#!/usr/bin/env bash
# 每个策略/负载/样本生成独立测试库与 WAL；性能与正确性使用同一正式 DAO 生成链路。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
label="${ROOST_PERF_LABEL:-dataengine-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || { echo 'Invalid label' >&2; exit 2; }
output="$repo_dir/artifacts/perf/dataengine/$label"
[[ ! -e "$output" ]] || { echo "Output already exists: $output" >&2; exit 2; }
repeats="${ROOST_PERF_COUNT:-3}"
[[ "$repeats" =~ ^[1-9][0-9]*$ ]] || { echo 'Invalid repeat count' >&2; exit 2; }
export GOMAXPROCS="${ROOST_PERF_CPU:-4}" GOWORK=off ROOST_DATAENGINE_PERF=1
[[ "$GOMAXPROCS" =~ ^[1-9][0-9]*$ ]] || { echo 'Invalid CPU count' >&2; exit 2; }
mkdir -p "$output"
cd "$repo_dir"
{
  go version
  git rev-parse HEAD
  git status --short
  printf 'GOMAXPROCS=%s\n' "$GOMAXPROCS"
} > "$output/env.txt"
for shape in ${ROOST_PERF_SHAPES:-single dual pair hot}; do
  case "$shape" in single|dual|pair|hot) ;; *) echo 'Invalid shape' >&2; exit 2;; esac
  for policy in ${ROOST_PERF_POLICIES:-async strict pipelined}; do
    case "$policy" in async|strict|pipelined) ;; *) echo 'Invalid policy' >&2; exit 2;; esac
    for ((sample=1; sample<=repeats; sample++)); do
      name="$shape-$policy-$sample"
      ROOST_PERF_SHAPE="$shape" ROOST_PERF_POLICY="$policy" ROOST_PERF_OUTPUT="$output/$name.json" \
        bash scripts/test-dataengine-generated.sh > "$output/$name.log" 2>&1
      cat "$output/$name.json"
    done
  done
done
printf 'Results: %s\n' "$output"
