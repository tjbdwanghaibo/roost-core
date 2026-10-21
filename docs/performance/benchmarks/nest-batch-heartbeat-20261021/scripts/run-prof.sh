#!/usr/bin/env bash
# profile 与正式测量分开：100% 移动、10ms；X / C / Y 各一次 CPU+mutex（10s 测量），C 另跑 20% 移动一次。
set -u
S="$1"; OUT="$2"; cd "$S"
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }
wait_quiet() { local q=0; while [ $q -lt 30 ]; do if [ -n "$("$S/foreign.sh" "$S")" ]; then q=0; else q=$((q+2)); fi; sleep 2; done; }
run1() { # label bin mode active extra-args...
  local label=$1 bin=$2 mode=$3 active=$4; shift 4
  while true; do
    wait_quiet; log "$label start loadavg=$(sysctl -n vm.loadavg)"
    HB_DELIVERY=1 HB_MODE=$mode HB_ACTIVE=$active HB_MEASURE_S=10 HB_LABEL="$label" HB_OUT="$OUT/profile-runs.jsonl" "./$bin" -test.run '^TestDeliveryRun$' -test.count=1 -test.timeout 0 "$@" > "$OUT/$label.out" 2>&1 &
    local pid=$! dirty=""
    while kill -0 $pid 2>/dev/null; do sleep 5; f="$("$S/foreign.sh" "$S")"; [ -n "$f" ] && dirty="$f"; done
    wait $pid
    if [ -n "$dirty" ]; then log "$label DISCARDED"; continue; fi
    log "$label ok"; break
  done
}
for m in "X bin-final.test x 1" "C bin-final.test c 1" "Y bin-final.test y 1" "X0 bin-base.test x 1" "C20 bin-final.test c 0.2"; do
  set -- $m
  run1 "$1" $2 $3 $4 -test.cpuprofile "$OUT/$1.cpu" -test.mutexprofile "$OUT/$1.mutex" -test.mutexprofilefraction 5
done
log "all done"
