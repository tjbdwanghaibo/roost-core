#!/usr/bin/env bash
# 批量作业组大小 / 时间片的同时段对照（100% 移动，轮内轮转），维护者 2026-10-22 决定 B 之后的补测。
# 二进制（gameplay/heartbeat/internal/bench 的 go test -c，见报告 §3F）：bin-final = 缺省（组 4、200µs）；
# bin-g8 = 组 8、200µs（2026-10-22 之前）；bin-s100 = 组 4、100µs；bin-s100g8 = 组 8、100µs。
# 门控同 run-formal（每次前等 30 秒无外来进程，运行中每 2 秒检查，采到就作废重跑），另加 foreign-ext.sh 的一条。
# 2026-10-22 上午停下的第 1 轮（stopped-round1.jsonl）用的是旧的组合与标签：C = 组 8 200µs、Cs100 = 组 8 100µs、
# Cg4 = 组 4 200µs、Cs100g4 = 组 4 100µs。
set -u
S="$1"; OUT="$2"; ROUNDS="${3:-3}"; cd "$S"; mkdir -p "$OUT"
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }
wait_quiet() { local q=0; while [ $q -lt 30 ]; do if [ -n "$("$S/foreign-ext.sh" "$S")" ]; then q=0; else q=$((q+2)); fi; sleep 2; done; }
COMBOS=("X bin-final.test x 1" "Cg4s200 bin-final.test c 1" "Cg8s200 bin-g8.test c 1" "Cg4s100 bin-s100.test c 1" "Cg8s100 bin-s100g8.test c 1" "none bin-final.test none 0")
for round in $(seq 1 "$ROUNDS"); do
  n=${#COMBOS[@]}
  for k in $(seq 0 $((n-1))); do
    set -- ${COMBOS[$(( (k + round*2) % n ))]}
    label=$1 bin=$2 mode=$3 active=$4
    while true; do
      wait_quiet; log "round=$round $label start loadavg=$(sysctl -n vm.loadavg)"
      tmp="$OUT/tmp.jsonl"; rm -f "$tmp"
      HB_DELIVERY=1 HB_MODE=$mode HB_ACTIVE=$active HB_RES=10ms HB_LABEL="$label/r$round" HB_OUT="$tmp" "./$bin" -test.run '^TestDeliveryRun$' -test.count=1 -test.timeout 0 > "$OUT/last.out" 2>&1 &
      pid=$!; dirty=""
      while kill -0 $pid 2>/dev/null; do sleep 2; f="$("$S/foreign-ext.sh" "$S")"; [ -n "$f" ] && dirty="$f"; done
      wait $pid; rc=$?
      if [ $rc -ne 0 ] || [ ! -s "$tmp" ]; then dirty="${dirty:-failed rc=$rc}"; fi
      if [ -n "$dirty" ]; then log "$label r$round DISCARDED: $(echo "$dirty" | head -1)"; continue; fi
      cat "$tmp" >> "$OUT/delivery.jsonl"; log "$label r$round ok loadavg_end=$(sysctl -n vm.loadavg)"; break
    done
  done
done
log "all done"
