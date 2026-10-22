#!/usr/bin/env bash
# policy 包基准：base / after 交替 PASSES 遍（每遍 -count=1），门控同 run-aoi.sh。
set -u
I="$1"; OUT="$2"; PASSES="${3:-10}"
cd "$I"; mkdir -p "$OUT"; export GOWORK=off
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }
wait_quiet() { local q=0; while [ $q -lt 10 ]; do if [ -n "$("$I/foreign.sh" "$I")" ]; then q=0; else q=$((q+2)); fi; sleep 2; done; }
for pass in $(seq 1 "$PASSES"); do
  order="base after"; [ $((pass%2)) -eq 0 ] && order="after base"
  for which in $order; do
    while true; do
      wait_quiet; log "pass=$pass $which start loadavg=$(sysctl -n vm.loadavg)"
      "$I/bin/policy-$which.test" -test.run '^$' -test.bench . -test.benchmem -test.count=1 -test.timeout 30m > "$OUT/tmp.txt" 2>&1 &
      pid=$!; dirty=""
      while kill -0 $pid 2>/dev/null; do sleep 3; f="$("$I/foreign.sh" "$I")"; [ -n "$f" ] && dirty="$f"; done
      wait $pid; rc=$?
      if [ $rc -ne 0 ] || [ -n "$dirty" ]; then log "pass=$pass $which DISCARDED rc=$rc $(echo "$dirty" | head -1)"; cat "$OUT/tmp.txt" >> "$OUT/discarded-$which.txt"; [ -z "$dirty" ] && break; continue; fi
      cat "$OUT/tmp.txt" >> "$OUT/$which.txt"; log "pass=$pass $which ok"; break
    done
  done
done
log "all done"
