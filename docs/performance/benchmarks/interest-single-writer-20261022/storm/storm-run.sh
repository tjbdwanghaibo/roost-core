#!/usr/bin/env bash
# storm-run.sh <工程目录> <loadtest 二进制> <机器人数> <first-player-id> <输出前缀>
# 起一次登录风暴：抓压测端指标（最后一次成功的抓取），结束后抓 game 进程的指标。
set -u
P="$1"; LT="$2"; N="$3"; FIRST="$4"; OUT="$5"
cd "$P"
"$LT" -count "$N" -scenario storm -first-player-id "$FIRST" -metrics-addr 127.0.0.1:9300 -account-nats nats://127.0.0.1:40222 -max-p95 60 > "$OUT.log" 2>&1 &
pid=$!
while kill -0 $pid 2>/dev/null; do
  if curl -s -m 1 127.0.0.1:9300/metrics > "$OUT.robot.tmp" 2>/dev/null && [ -s "$OUT.robot.tmp" ]; then mv "$OUT.robot.tmp" "$OUT.robot.metrics"; fi
  sleep 0.3
done
wait $pid; echo "loadtest rc=$?" >> "$OUT.log"
curl -s -m 5 127.0.0.1:9100/metrics > "$OUT.game.metrics"
