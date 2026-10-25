#!/usr/bin/env bash
# policy 微基准三点交替：每轮 p0 / p1 / p2 各跑 -test.count=1，起点按轮轮换；门控同 run-list.sh（作废重跑）。
set -u
P="$1"; OUT="$2"; ROUNDS="${3:-10}"; mkdir -p "$OUT"; cd "$P/bin"
export GOWORK=off
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }
wait_quiet() { local q=0; while [ $q -lt 10 ]; do if [ -n "$("$P/cdefault/foreign-ext.sh" "$P")" ]; then q=0; else q=$((q+2)); fi; sleep 2; done; }
PTS=(p0 p1 p2)
for round in $(seq 1 "$ROUNDS"); do
  for k in 0 1 2; do
    n=${PTS[$(( (k + round) % 3 ))]}
    while true; do
      wait_quiet; log "r$round $n start loadavg=$(sysctl -n vm.loadavg)"
      "$P/bin/policy-$n.test" -test.run '^$' -test.bench 'AOI|Cluster|View|Relation' -test.benchmem -test.count=1 -test.timeout 0 > "$OUT/tmp.txt" 2>&1 &
      pid=$!; dirty=""
      while kill -0 $pid 2>/dev/null; do sleep 2; f="$("$P/cdefault/foreign-ext.sh" "$P")"; [ -n "$f" ] && dirty="$f"; done
      wait $pid; rc=$?
      [ $rc -ne 0 ] && dirty="${dirty:-rc=$rc}"
      if [ -n "$dirty" ]; then log "r$round $n DISCARDED: $(echo "$dirty" | head -1)"; continue; fi
      cat "$OUT/tmp.txt" >> "$OUT/policy-$n.txt"; log "r$round $n ok loadavg_end=$(sysctl -n vm.loadavg)"; break
    done
  done
done
log "all done"
