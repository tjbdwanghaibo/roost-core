#!/usr/bin/env bash
# Interest 单写者前后对照（sync-aoi）：base = 494134ae，after = 当前 main；ApplySync 16 视野两边都用
# SyncApplyMaxExtraViews=16 的诊断构建。每次运行前等 10 秒没有外来 go 进程，运行中每 2 秒检查并采样服务端 / 客户端 CPU，
# 采到外来进程就作废重跑。轮内按轮转顺序，每个配置内 base / after 先后按轮交替。
set -u
I="$1"; OUT="$2"; ROUNDS="${3:-2}"; SCENE="${4:-o}"
cd "$I"; mkdir -p "$OUT/runs"
export GOMAXPROCS=4 GOWORK=off
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }
wait_quiet() { local q=0; while [ $q -lt 10 ]; do if [ -n "$("$I/foreign.sh" "$I")" ]; then q=0; else q=$((q+2)); fi; sleep 2; done; }
BASEARGS="-players=1000 -entities=10000 -hz=20 -dirty=1 -visible=50"
if [ "$SCENE" = n ]; then
  BASEARGS="$BASEARGS -world=4000 -block=20 -enter-radius=30 -leave-radius=36 -move-amplitude=10 -entities=50000"
  VIEWS="4 8"
else
  VIEWS="0 4 8 16"
fi
CONFIGS=()
for v in $VIEWS; do for a in sync async; do for d in nest direct; do CONFIGS+=("$v $a $d"); done; done; done
for round in $(seq 1 "$ROUNDS"); do
  n=${#CONFIGS[@]}
  for k in $(seq 0 $((n-1))); do
    set -- ${CONFIGS[$(( (k + round*5) % n ))]}
    v=$1 a=$2 d=$3
    order="base after"; [ $(( (round + k) % 2 )) -eq 0 ] && order="after base"
    for which in $order; do
      bin="$I/bin/aoi-$which"; [ "$v" = 16 ] && [ "$a" = sync ] && bin="$I/bin/aoi-$which-d16"
      nest=true; [ "$d" = direct ] && nest=false
      name="$SCENE-v$v-$a-$d-$which-r$round"
      while true; do
        wait_quiet
        rm -rf "$OUT/runs/$name"
        log "$name start loadavg=$(sysctl -n vm.loadavg)"
        "$bin" $BASEARGS -views-per-player=$v -apply-mode=$a -nest=$nest -output="$OUT/runs/$name" > "$OUT/runs/$name.log" 2>&1 &
        pid=$!; dirty=""; : > "$OUT/runs/$name.cpu"
        while kill -0 $pid 2>/dev/null; do
          sleep 2
          ps -Ao pid=,time=,args= | awk -v b="$I/bin/aoi-" 'index($0,b)>0 {role="server"; if (index($0,"-role=client")>0 || index($0,"-role client")>0) role="client"; print $1, role, $2}' >> "$OUT/runs/$name.cpu"
          f="$("$I/foreign.sh" "$I")"; [ -n "$f" ] && dirty="$f"
        done
        wait $pid; rc=$?
        if [ $rc -ne 0 ] && [ -z "$dirty" ]; then log "$name FAILED rc=$rc"; tail -3 "$OUT/runs/$name.log" | tee -a "$OUT/run.log"; echo "$name" >> "$OUT/failed.txt"; break; fi
        if [ -n "$dirty" ]; then log "$name DISCARDED: $(echo "$dirty" | head -1)"; mv "$OUT/runs/$name" "$OUT/runs/$name.discarded.$(date +%s)" 2>/dev/null; continue; fi
        log "$name ok"; break
      done
    done
  done
done
log "all done"
