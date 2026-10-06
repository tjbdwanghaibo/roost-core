# 演练 1（单 sid）：单实例锁等待 / 接管、失锁 fail-stop、崩溃重启、优雅停机。用法：DRILL_HOME=<私有目录> CORE_DIR=<roost-core> bash drill1.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
mark "=== drill 1 start (core $(git -C "$CORE_DIR" rev-parse --short HEAD)$(git -C "$CORE_DIR" diff --quiet HEAD -- . || echo +dirty))"
mark "=== step 0: P1 up + robots"
start_game P1 1000 config.game.yaml
wait_ready P1 $GAME_OPS_PORT 60
loadtest A 127.0.0.1:36700 1000 10 100000
loadtest A2 127.0.0.1:36700 1000 10 100010
sleep 3
mark "players in mongo: $(players $D/players-after-A.txt; wc -l < $D/players-after-A.txt)"

mark "=== step 1: SIGSTOP P1 ($(pidof P1))"
kill -STOP $(pidof P1); mark "SIGSTOP P1; $(key 1000)"
players $D/players-at-stop.txt; mark "players in mongo at stop: $(wc -l < $D/players-at-stop.txt)"
start_game P2 1000 config.game.yaml
wait_exit P2 40
mark "after P2: $(key 1000)"
sleep 2
mark "=== step 2: SIGCONT P1"
kill -CONT $(pidof P1)
wait_exit P1 20
mark "after P1 exit: $(key 1000)"

mark "=== step 3: P2b takes over"
start_game P2b 1000 config.game.yaml
wait_ready P2b $GAME_OPS_PORT 60
sleep 1
players $D/players-after-takeover.txt; mark "players after takeover: $(wc -l < $D/players-after-takeover.txt)"
cmp -s $D/players-at-stop.txt $D/players-after-takeover.txt && mark "takeover snapshot identical to stop snapshot" || mark "takeover snapshot DIFFERS from stop snapshot"
loadtest B 127.0.0.1:36700 1000 10 100020
sleep 3
players $D/players-after-B.txt; mark "players after B: $(wc -l < $D/players-after-B.txt)"

mark "=== step 4: kill -9 P2b; $(key 1000)"
kill -9 $(pidof P2b)
start_game P3 1000 config.game.yaml
wait_ready P3 $GAME_OPS_PORT 60

mark "=== step 5: SIGTERM P3, start P4 immediately"
kill -TERM $(pidof P3)
start_game P4 1000 config.game.yaml
wait_exit P3 30
wait_ready P4 $GAME_OPS_PORT 60
mark "=== step 5b: SIGTERM P4, start P5 after P4 exits"
kill -TERM $(pidof P4)
wait_exit P4 30
mark "after P4 exit: $(key 1000)"
start_game P5 1000 config.game.yaml
wait_ready P5 $GAME_OPS_PORT 60

mark "=== step 1c/2c: idle robots on P5, SIGSTOP, P6, SIGCONT"
mark "loadtest idle start (background, enter_game then idle)"
( bin/loadtest -endpoint 127.0.0.1:36700 -count 4 -account-nats "$ROOST_DATAENGINE_IT_NATS_URL" -server-id 1000 -first-player-id 100100 -scenario idle -scenarios $D/scenarios-idle -duration 120s > $D/logs/loadtest-idle.log 2>&1; echo "idle rc=$?" >> $D/logs/loadtest-idle.log ) &
sleep 6
mark "tcp before stop: $(curl -s --max-time 1 http://127.0.0.1:$GAME_OPS_PORT/metrics | grep '^player_tcp_connections')"
kill -STOP $(pidof P5); mark "SIGSTOP P5; $(key 1000)"
start_game P6 1000 config.game.yaml
wait_exit P6 40
kill -CONT $(pidof P5); mark "SIGCONT P5"
wait_exit P5 20
mark "after P5 exit: $(key 1000)"
start_game P7 1000 config.game.yaml
wait_ready P7 $GAME_OPS_PORT 60
mark "=== drill 1 single-sid part done"
