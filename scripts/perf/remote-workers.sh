#!/usr/bin/env bash
# 对比慢池并发/等待预算：业务错误继续下一档，环境或预热失败立即停止。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
label="${ROOST_REMOTE_WORKER_LABEL:-workers-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || { echo 'Invalid label' >&2; exit 2; }
cases="${ROOST_REMOTE_WORKER_CASES:-64:64:120 1024:16:120 128:16:120 256:16:120 64:16:120 1024:64:120}"
for spec in $cases; do
  [[ "$spec" =~ ^[1-9][0-9]*:[1-9][0-9]*:[1-9][0-9]*$ ]] || { echo "Invalid workers:queue:rate: $spec" >&2; exit 2; }
done
output="$repo_dir/artifacts/perf/remote/$label"
mkdir -p "$(dirname "$output")"
mkdir "$output" # 已有结果不可覆盖。
printf '%s\n' "$cases" > "$output/cases.txt"
printf 'workers\tqueue\trate\texit\tresult\n' > "$output/results.tsv"
export ROOST_REMOTE_WORKERS="${ROOST_REMOTE_WORKERS:-8}"
export ROOST_REMOTE_FAST_QUEUE="${ROOST_REMOTE_FAST_QUEUE:-4096}"
export ROOST_REMOTE_PROJECTION_WORKERS="${ROOST_REMOTE_PROJECTION_WORKERS:-8}"
export ROOST_REMOTE_DURATION="${ROOST_REMOTE_DURATION:-120s}"
export ROOST_REMOTE_TIMEOUT="${ROOST_REMOTE_TIMEOUT:-10m}"
export ROOST_REMOTE_STAGE_METRICS=1
failed=0
for spec in $cases; do
  IFS=: read -r workers queue rate <<< "$spec"
  case_label="$label-w$workers-q$queue-r$rate"
  case_output="$repo_dir/artifacts/perf/remote/$case_label"
  [[ ! -e "$case_output" ]] || { echo "Output already exists: $case_output" >&2; exit 2; }
  printf 'Starting %s\n' "$spec"
  set +e
  ROOST_REMOTE_LABEL="$case_label" ROOST_REMOTE_IO_WORKERS="$workers" \
    ROOST_REMOTE_SLOW_QUEUE="$queue" ROOST_REMOTE_RATE="$rate" \
    bash "$repo_dir/scripts/perf/remote.sh" > "$output/$case_label.log" 2>&1
  code=$?
  set -e
  printf '%s\t%s\t%s\t%s\t%s\n' "$workers" "$queue" "$rate" "$code" "$case_label" >> "$output/results.tsv"
  printf 'Finished %s exit=%s\n' "$spec" "$code"
  if [[ -d "$case_output" ]]; then printf '%s\n' "$code" > "$case_output/exit-code.txt"; fi
  [[ -f "$case_output/result.json" ]] || { echo 'No measured result: inspect environment/warmup logs' >&2; exit 2; }
  if (( code != 0 )); then failed=1; fi
done
exit "$failed"
