#!/usr/bin/env bash
# 补充轮（100% 移动，同一时段内轮转）：X / Y / C（g8 s200 b64）/ C-s100 / C-g4 / C-g16 / none。正式表的第 3 轮起宿主状态变化
# （无心跳基线 0.05 → 0.22 核），同一时段内的相对位置要再确认；也补组大小对照。门控同 run-formal。
set -u
S="$1"; OUT="$2"; ROUNDS="${3:-3}"; cd "$S"; mkdir -p "$OUT"
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }
wait_quiet() { local q=0; while [ $q -lt 30 ]; do if [ -n "$("$S/foreign.sh" "$S")" ]; then q=0; else q=$((q+2)); fi; sleep 2; done; }
COMBOS=("X bin-final.test x 1" "Y bin-final.test y 1" "C bin-final.test c 1" "Cs100 bin-s100.test c 1" "Cg4 bin-g4.test c 1" "Cg16 bin-g16.test c 1" "none bin-final.test none 0")
for round in $(seq 1 "$ROUNDS"); do
  n=${#COMBOS[@]}
  for k in $(seq 0 $((n-1))); do
    set -- ${COMBOS[$(( (k + round*3) % n ))]}
    label=$1 bin=$2 mode=$3 active=$4
    while true; do
      wait_quiet; log "round=$round $label start loadavg=$(sysctl -n vm.loadavg)"
      tmp="$OUT/tmp.jsonl"; rm -f "$tmp"
      HB_DELIVERY=1 HB_MODE=$mode HB_ACTIVE=$active HB_RES=10ms HB_LABEL="$label/r$round" HB_OUT="$tmp" "./$bin" -test.run '^TestDeliveryRun$' -test.count=1 -test.timeout 0 > "$OUT/last.out" 2>&1 &
      pid=$!; dirty=""
      while kill -0 $pid 2>/dev/null; do sleep 2; f="$("$S/foreign.sh" "$S")"; [ -n "$f" ] && dirty="$f"; done
      wait $pid; rc=$?
      if [ $rc -ne 0 ] || [ ! -s "$tmp" ]; then dirty="${dirty:-failed rc=$rc}"; fi
      if [ -n "$dirty" ]; then log "$label r$round DISCARDED: $(echo "$dirty" | head -1)"; continue; fi
      cat "$tmp" >> "$OUT/delivery.jsonl"; log "$label r$round ok"; break
    done
  done
done
log "all done"
