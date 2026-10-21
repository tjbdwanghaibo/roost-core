#!/usr/bin/env bash
# profile 与正式测量分开：100% 移动、10ms；每个模式一次 CPU+mutex（10s 测量）与一次执行追踪（1s 测量）。
set -u
S="$1"; OUT="$2"; cd "$S"
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }
wait_quiet() { local q=0; while [ $q -lt 30 ]; do if [ -n "$("$S/foreign.sh" "$S")" ]; then q=0; else q=$((q+2)); fi; sleep 2; done; }
run1() { # label bin mode extra-args...
  local label=$1 bin=$2 mode=$3; shift 3
  while true; do
    wait_quiet; log "$label start"
    HB_DELIVERY=1 HB_MODE=$mode HB_ACTIVE=1 HB_LABEL="$label" HB_OUT="$OUT/profile-runs.jsonl" "./$bin" -test.run '^TestDeliveryRun$' -test.count=1 -test.timeout 0 "$@" > "$OUT/$label.out" 2>&1 &
    local pid=$! dirty=""
    while kill -0 $pid 2>/dev/null; do sleep 5; f="$("$S/foreign.sh" "$S")"; [ -n "$f" ] && dirty="$f"; done
    wait $pid
    if [ -n "$dirty" ]; then log "$label DISCARDED"; continue; fi
    log "$label ok"; break
  done
}
for m in "X0 bin-base.test x" "X bin-final.test x" "D bin-final.test d" "Y bin-final.test y" "D+shard bin-shard.test d"; do
  set -- $m; l=$1; b=$2; md=$3; f=${l/+/-}
  HB_MEASURE_S=10 run1 "$l-cpu" $b $md -test.cpuprofile "$OUT/$f.cpu" -test.mutexprofile "$OUT/$f.mutex" -test.mutexprofilefraction 5
done
for m in "X bin-final.test x" "D bin-final.test d" "Y bin-final.test y" "D+shard bin-shard.test d" "X+shard bin-shard.test x"; do
  set -- $m; l=$1; b=$2; md=$3; f=${l/+/-}
  HB_WARM_S=2 HB_MEASURE_S=1 run1 "$l-trace" $b $md -test.trace "$OUT/$f.trace"
done
log "all done"
