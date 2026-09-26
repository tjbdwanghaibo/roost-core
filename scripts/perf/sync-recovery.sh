#!/usr/bin/env bash
# 同一正式 AOI 负载、同一 50ms 门禁，逐级测量冷恢复预算。失败样本也继续保留。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
label="${ROOST_PERF_LABEL:-recovery-$(date +%Y%m%d-%H%M%S)}"
status=0
for budget in ${ROOST_RECOVERY_BUDGETS:-1000 2000 5000}; do
 [[ "$budget" =~ ^[1-9][0-9]*$ ]] || { echo 'Invalid recovery budget' >&2; exit 2; }
 for dirty in 1 5; do
  ROOST_PERF_LABEL="$label-b$budget-d$dirty" bash "$repo_dir/scripts/perf/sync-aoi.sh" \
   -nest=true -mode=on_change -async=true -players=1000 -entities=10000 -visible=50 \
   -hz=20 -batches=10 -ticks=200 -dirty="$dirty" -snapshot-objects="$budget" \
   -snapshot-per-session=10 -reconnect-tick=50 -reconnect-players=1000 "$@" || status=1
 done
done
exit "$status"
