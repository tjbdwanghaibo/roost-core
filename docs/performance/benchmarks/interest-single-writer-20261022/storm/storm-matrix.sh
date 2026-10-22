#!/usr/bin/env bash
# 登录风暴前后对照：after = hbsmoke（main），base = hbbase（494134ae）；每次运行重启十个进程（指标从零开始），
# 80 个机器人（更多时玩家全挤在出生点、持有对象超过 Manager MaxObjects 100，复制会话被关闭），3 轮、轮内先后交替。
set -u
G="$1"; ROUNDS="${2:-3}"; N="${3:-80}"
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$G/storm/run.log"; }
wait_quiet() { local q=0; while [ $q -lt 10 ]; do if [ -n "$("$G/foreign.sh" "$G")" ]; then q=0; else q=$((q+2)); fi; sleep 2; done; }
mkdir -p "$G/storm"
first=5000000
for round in $(seq 1 "$ROUNDS"); do
  order="after base"; [ $((round%2)) -eq 0 ] && order="base after"
  for which in $order; do
    proj=hbsmoke; [ $which = base ] && proj=hbbase
    (cd "$G/$proj" && GOWORK=off GOFLAGS=-mod=mod sh deploy/dev/run.sh start > "$G/storm/$which-r$round.devrun.log" 2>&1) || { log "$which r$round dev-run failed"; continue; }
    sleep 3; wait_quiet
    log "$which r$round start loadavg=$(sysctl -n vm.loadavg)"
    "$G/storm-run.sh" "$G/$proj" "$G/loadtest-$which" "$N" "$first" "$G/storm/$which-r$round"
    first=$((first+100000))
    cp "$G/$proj/.dev/game.log" "$G/storm/$which-r$round.game.log"
    log "$which r$round done $(tail -1 "$G/storm/$which-r$round.log") object_limit=$(grep -c 'object limit' "$G/$proj/.dev/game.log")"
    (cd "$G/$proj" && sh deploy/dev/run.sh stop > /dev/null 2>&1)
    sleep 3
  done
done
log "all done"
