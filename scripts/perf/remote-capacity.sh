#!/usr/bin/env bash
# 每一档重新生成并全量验收；首次失败保留现场并停止，不能自动把失败档当通过。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
label="${ROOST_REMOTE_CAPACITY_LABEL:-capacity-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || { echo 'Invalid label' >&2; exit 2; }
export ROOST_REMOTE_DURATION="${ROOST_REMOTE_DURATION:-120s}"
export ROOST_REMOTE_STAGE_METRICS=1
for rate in ${ROOST_REMOTE_RATES:-80 120 160 200 240}; do
  [[ "$rate" =~ ^[1-9][0-9]*$ ]] || { echo 'Invalid rate' >&2; exit 2; }
  set +e
  ROOST_REMOTE_LABEL="$label-$rate" ROOST_REMOTE_RATE="$rate" bash "$repo_dir/scripts/perf/remote.sh"
  case_code=$?
  set -e
  output="$repo_dir/artifacts/perf/remote/$label-$rate"
  if [[ -d "$output" && ! -e "$output/exit-code.txt" ]]; then
    printf '%s\n' "$case_code" > "$output/exit-code.txt"
  fi
  printf 'rate=%s exit=%s results=%s\n' "$rate" "$case_code" "$output"
  if (( case_code != 0 )); then exit "$case_code"; fi
done
