#!/usr/bin/env bash
# 批大小 / 时间片对照：100% 移动，C 组；HB_BATCH 32/64/128（时间片 200µs），时间片 100/400µs（批 64，改常量重编）；轮内轮转，门控同 run-formal。
set -u
S="$1"; OUT="$2"; ROUNDS="${3:-3}"; cd "$S"
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }
wait_quiet() { local q=0; while [ $q -lt 30 ]; do if [ -n "$("$S/foreign.sh" "$S")" ]; then q=0; else q=$((q+2)); fi; sleep 2; done; }
COMBOS=("b32 bin-final.test 32" "b64 bin-final.test 64" "b128 bin-final.test 128" "s100 bin-s100.test 64" "s400 bin-s400.test 64")
for round in $(seq 1 "$ROUNDS"); do
  n=${#COMBOS[@]}
  for k in $(seq 0 $((n-1))); do
    set -- ${COMBOS[$(( (k + round) % n ))]}
    label=$1 bin=$2 batch=$3
    while true; do
      wait_quiet; log "round=$round $label start loadavg=$(sysctl -n vm.loadavg)"
      tmp="$OUT/tmp.jsonl"; rm -f "$tmp"
      HB_DELIVERY=1 HB_MODE=c HB_BATCH=$batch HB_ACTIVE=1 HB_RES=10ms HB_LABEL="$label/r$round" HB_OUT="$tmp" "./$bin" -test.run '^TestDeliveryRun$' -test.count=1 -test.timeout 0 > "$OUT/last.out" 2>&1 &
      pid=$!; dirty=""
      while kill -0 $pid 2>/dev/null; do sleep 5; f="$("$S/foreign.sh" "$S")"; [ -n "$f" ] && dirty="$f"; done
      wait $pid; rc=$?
      if [ $rc -ne 0 ] || [ ! -s "$tmp" ]; then dirty="failed rc=$rc"; fi
      if [ -n "$dirty" ]; then log "$label r$round DISCARDED: $(echo "$dirty" | head -1)"; continue; fi
      cat "$tmp" >> "$OUT/delivery.jsonl"; log "$label r$round ok"; break
    done
  done
done
log "all done"
