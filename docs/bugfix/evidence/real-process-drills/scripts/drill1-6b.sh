#!/usr/bin/env bash
# 演练 1 第 6b 步（APP-1 / OWN-3）：sid 1000 上赠礼 saga 在途时 kill -9 当前进程、立刻起新进程，在途 saga 由新进程
# 按 FromSID 接手到终态（sid 1001 的 Q1 一直在跑）。
# 用法：DRILL_HOME=<私有目录> bash drill1-6b.sh <当前 sid 1000 进程名> <新进程名> [首个玩家 ID]
# 旧版在机器人开始后固定 4.5s kill；当前代码赠礼更快，4.5s 时已全部终结，改为轮询到出现在途 saga 再 kill。
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
OLD=${1:?running process name} NEW=${2:?new process name} FIRST=${3:-100500}
SINCE6B=$(date -u +%Y-%m-%dT%H:%M:%SZ)
mark "=== step 6b: gifts in flight on 1000, kill -9 $OLD, start $NEW at once (since $SINCE6B)"
( loadtest F 127.0.0.1:36700 1000 10 "$FIRST" ) & LF=$!
s=""
for _ in $(seq 1 60); do
  s=$(sagasince "$SINCE6B")
  echo "$s" | grep -q 'nonterminal=0 ' || { echo "$s" | grep -q 'total=0 ' || break; }
  perl -e 'select undef,undef,undef,0.1'
done
mark "sagas at kill: $s"
kill -9 "$(pidof "$OLD")"; mark "kill -9 $OLD; $(key 1000)"
start_game "$NEW" 1000 config.game.yaml
wait $LF
wait_ready "$NEW" "$GAME_OPS_PORT" 60
for i in $(seq 1 12); do sleep 5; s=$(sagasince "$SINCE6B"); mark "sagas since 6b (+$((i*5))s): $s"; echo "$s" | grep -q "nonterminal=0 " && break; done
mark "$NEW ran handed-over steps: $(grep -c 'handed over by another process' "$D/logs/$NEW.log"); Q1 left-to-sender: $(grep -c "left to the sender's sid" "$D/logs/Q1.log")"
mark "manual_required in saga db: $(mq "print(db.getSiblingDB('saga').getCollection('_sagas').countDocuments({status:{\$gte:6}}))")"
