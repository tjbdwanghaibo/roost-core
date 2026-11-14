#!/bin/bash
# usage: sampler.sh <outfile> <seconds>
out=$1; dur=${2:-360}; end=$(( $(date +%s) + dur ))
while [ $(date +%s) -lt $end ]; do
  ts=$(python3 -c 'import time;print(f"{time.time():.3f}")')
  for p in 9100 9103; do
    curl -s -m 1 http://127.0.0.1:$p/metrics 2>/dev/null | grep -E '^(nest_handler_lock_hold_(count|sum_nanos)\{handler="(handlerRecordEnter|handlerEnterInit|cellworld_base_enter)|nest_dispatch_queue_len|nestwal_fsync_duration_(count|sum)|player_tcp_dispatch_timeout_total|player_tcp_connections|versionstore|chat)' | sed "s/^/$ts $p /" >> $out
  done
  sleep 0.5
done
