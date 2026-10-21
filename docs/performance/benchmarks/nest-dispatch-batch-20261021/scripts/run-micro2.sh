#!/usr/bin/env bash
# 普通短 handler 的影响：base（be5a76f5）与 final 交替，每遍前等 30 秒安静，运行中采到外来 go 进程即作废重跑。
set -u
S="$1"; OUT="$2"; PASSES="${3:-6}"
cd "$S"
BENCHES=(
  '^BenchmarkDispatchCompare$/^noop$/^(uniform|colliding_ids|hot_id)$/^nest$'
  '^BenchmarkDispatchCompareMicro$/^cpu_(10|50)us$/^uniform$/^nest$'
  '^BenchmarkDispatchCompareProducers$/^p(1|16)$/^nest$'
  '^BenchmarkClientRequestSingle$'
  '^BenchmarkNestHotPath$/^cpu=false$/^workers=8$/^independent=true$/^single$/^async=(true|false)$'
)
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }
wait_quiet() { local q=0; while [ $q -lt 30 ]; do if [ -n "$("$S/foreign.sh" "$S")" ]; then q=0; else q=$((q+2)); fi; sleep 2; done; }
for pass in $(seq 1 "$PASSES"); do
  order="base final2"; [ $((pass%2)) -eq 0 ] && order="final2 base"
  for which in $order; do
    while true; do
      wait_quiet
      log "pass=$pass $which start loadavg=$(sysctl -n vm.loadavg)"
      ( for b in "${BENCHES[@]}"; do ./nest-$which.test -test.run '^$' -test.bench "$b" -test.benchmem -test.benchtime=1s -test.count=1 -test.cpu=4,10 -test.timeout 30m || exit 1; done ) > "$OUT/tmp.txt" 2>&1 &
      pid=$!; dirty=""
      while kill -0 $pid 2>/dev/null; do sleep 5; f="$("$S/foreign.sh" "$S")"; [ -n "$f" ] && dirty="$f"; done
      wait $pid; rc=$?
      if [ $rc -ne 0 ] || [ -n "$dirty" ]; then log "pass=$pass $which DISCARDED rc=$rc $(echo "$dirty" | head -1)"; cat "$OUT/tmp.txt" >> "$OUT/discarded-$which.txt"; continue; fi
      cat "$OUT/tmp.txt" >> "$OUT/$which.txt"; log "pass=$pass $which ok"; break
    done
  done
done
log "all done"
