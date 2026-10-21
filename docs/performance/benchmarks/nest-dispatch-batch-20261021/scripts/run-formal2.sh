#!/usr/bin/env bash
# 正式对比：3 轮，每轮按轮转顺序跑全部组合；每次运行前等 30 秒无外来 go 进程，运行中每 5 秒检查，采到就作废重跑。
set -u
S="$1"; OUT="$2"; ROUNDS="${3:-3}"
cd "$S"
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }
wait_quiet() {
  local quiet=0
  while [ $quiet -lt 30 ]; do
    if [ -n "$("$S/foreign.sh" "$S")" ]; then
      [ $quiet -gt 0 ] && log "foreign seen, reset quiet counter"; quiet=0
    else
      quiet=$((quiet+2))
    fi
    sleep 2
  done
}
# 组合：标签 二进制 模式
COMBOS=(
  "X bin-final2.test x"
  "Y bin-final2.test y"
  "D bin-final2.test d"
  "D+shard bin-shard2.test d"
  "X+shard bin-shard2.test x"
)
for round in $(seq 1 "$ROUNDS"); do
  n=${#COMBOS[@]}
  for active in 0.2 1; do
    off=$((round*2)); [ "$active" = "1" ] && off=$((off+3))
    list=()
    for k in $(seq 0 $((n-1))); do list+=("${COMBOS[$(( (k + off) % n ))]}"); done
    for combo in "${list[@]}"; do
      set -- $combo; label=$1; bin=$2; mode=$3
      while true; do
        wait_quiet
        log "round=$round active=$active $label start loadavg=$(sysctl -n vm.loadavg)"
        tmp="$OUT/tmp.jsonl"; rm -f "$tmp"
        HB_DELIVERY=1 HB_MODE=$mode HB_ACTIVE=$active HB_RES=10ms HB_LABEL="$label/r$round" HB_OUT="$tmp" \
          "./$bin" -test.run '^TestDeliveryRun$' -test.count=1 -test.timeout 0 > "$OUT/last.out" 2>&1 &
        pid=$!
        dirty=""
        while kill -0 $pid 2>/dev/null; do
          sleep 5
          f="$("$S/foreign.sh" "$S")"
          if [ -n "$f" ]; then dirty="$f"; fi
        done
        wait $pid; rc=$?
        if [ $rc -ne 0 ] || [ ! -s "$tmp" ]; then log "$label run failed rc=$rc"; tail -5 "$OUT/last.out" | tee -a "$OUT/run.log"; dirty="failed"; fi
        if [ -n "$dirty" ]; then
          log "$label r$round active=$active DISCARDED: $(echo "$dirty" | head -2 | tr '\n' ';')"
          [ -s "$tmp" ] && sed 's/^/{"discarded":true,"row":/; s/$/}/' "$tmp" >> "$OUT/discarded.jsonl"
          continue
        fi
        cat "$tmp" >> "$OUT/delivery.jsonl"
        log "$label r$round active=$active ok"
        break
      done
    done
  done
  # 无心跳基线每轮一次
  while true; do
    wait_quiet
    tmp="$OUT/tmp.jsonl"; rm -f "$tmp"
    log "round=$round none start loadavg=$(sysctl -n vm.loadavg)"
    HB_DELIVERY=1 HB_MODE=none HB_LABEL="none/r$round" HB_OUT="$tmp" ./bin-final2.test -test.run '^TestDeliveryRun$' -test.count=1 -test.timeout 0 > "$OUT/last.out" 2>&1 &
    pid=$!; dirty=""
    while kill -0 $pid 2>/dev/null; do sleep 5; f="$("$S/foreign.sh" "$S")"; [ -n "$f" ] && dirty="$f"; done
    wait $pid
    if [ -n "$dirty" ] || [ ! -s "$tmp" ]; then log "none r$round DISCARDED"; continue; fi
    cat "$tmp" >> "$OUT/delivery.jsonl"; log "none r$round ok"; break
  done
done
log "all done"
