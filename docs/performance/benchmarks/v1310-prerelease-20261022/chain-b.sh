#!/usr/bin/env bash
# 第 6 项后半：nest-game ticker / heartbeat 各 15 分钟（V1290 重档参数），驱动微基准，开环投递。
set -u
P=<scratch>; W=<worktree>; O=$P/item6b
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$O/run.log"; }
wait_quiet() { local q=0; while [ $q -lt 30 ]; do if [ -n "$("$P/cdefault/foreign-ext.sh" "$P")" ]; then q=0; else q=$((q+2)); fi; sleep 2; done; }
hostload() { while true; do echo "$(date +%H:%M:%S) $(sysctl -n vm.loadavg) | $(ps -Ao pcpu=,comm= -r | head -6 | awk '{printf "%s %s; ", $1, $2}')" >> "$O/host-load.txt"; sleep 15; done; }
hostload & HL=$!
for driver in ticker heartbeat; do
  wait_quiet; log "nest-game $driver start loadavg=$(sysctl -n vm.loadavg)"
  (cd $W && ROOST_PERF_LABEL=p4-game-$driver-20261022 ROOST_PERF_COUNT=1 ROOST_PERF_CPU=0 \
   ROOST_NEST_GAME_HEARTBEAT_DRIVER=$driver \
   ROOST_NEST_GAME_DIRTY=5 ROOST_NEST_GAME_DURATION=15m ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_EMBEDDED=1 ROOST_NEST_GAME_HZ=10 \
   ROOST_NEST_GAME_IO_WORKERS=8 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_PROFILE=0 \
   ROOST_NEST_GAME_RACE=0 ROOST_NEST_GAME_SHORT_QUEUE=65536 ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_SYNC=1 \
   bash scripts/perf/nest-game.sh > "$O/nest-game-$driver.out" 2>&1); log "nest-game $driver rc=$? loadavg_end=$(sysctl -n vm.loadavg)"
done
cd $P/cdefault
wait_quiet; log "driver bench start loadavg=$(sysctl -n vm.loadavg)"
GOWORK=off ./bin-final.test -test.run '^$' -test.bench BenchmarkDriverDispatch -test.benchtime 2000000x -test.count 10 -test.timeout 0 > "$O/driver.txt" 2>&1; log "driver bench rc=$? loadavg_end=$(sysctl -n vm.loadavg)"
for r in 1 2; do
  if [ $r = 1 ]; then rates="100000 250000 500000"; else rates="500000 250000 100000"; fi
  for rate in $rates; do
    wait_quiet; log "openloop $rate r$r start loadavg=$(sysctl -n vm.loadavg)"
    HB_OPENLOOP=1 HB_RATE=$rate HB_PERIOD=100ms HB_WORKERS=8 HB_QUEUE=65536 HB_SECONDS=20 HB_LABEL=openloop-$rate-r$r HB_OUT="$O/openloop.jsonl" \
      ./bin-final.test -test.run '^TestDriverOpenLoop$' -test.count=1 -test.timeout 0 -test.v > "$O/openloop-$rate-r$r.log" 2>&1
    log "openloop $rate r$r rc=$? loadavg_end=$(sysctl -n vm.loadavg)"
  done
done
kill $HL
log "all done"
