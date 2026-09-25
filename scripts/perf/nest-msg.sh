#!/usr/bin/env bash
set -euo pipefail
readonly repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly label="${ROOST_PERF_LABEL:-msg-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || { echo 'Invalid label' >&2; exit 2; }
readonly output="$repo_dir/artifacts/perf/nest/$label"
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
  printf 'GOMAXPROCS=%s\n' "$GOMAXPROCS"
  printf 'args:'; printf ' %q' "$@"; printf '\n'
} > "$output/env.txt"
go build -o "$output/nest-msg" ./scripts/perf/nest-msg
# 每个样本使用独立进程，避免上次的 GC、指标注册表或热补丁状态进入下一轮。
for ((sample=1; sample<=repeats; sample++)); do
  "$output/nest-msg" "$@" > "$output/sample-$sample.json" 2> "$output/sample-$sample.log"
  cat "$output/sample-$sample.json"
done
printf 'Results: %s\n' "$output"
