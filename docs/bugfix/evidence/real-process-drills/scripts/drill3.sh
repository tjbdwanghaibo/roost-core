#!/usr/bin/env bash
# 演练 3（NC-231 / APP-8）：停机 hook 卡住时 SIGTERM。config.game.hook.yaml 把 shutdown.total_timeout 缩到 10s；
# ROOST_DRILL_STUCK_HOOK 让生成工程登记一个不配合 ctx 的停机 hook（drill_hooks.go.txt）。
# 期望：service.stopping 卡住——run 在预算（10s）附近返回、非零退出，不调 Service.Shutdown、不停 Mod、不释放锁（键在 ttl
# 内过期），日志点出卡住的 hook；service.stopped 卡住——Mod 已停完，run 同样按预算返回。
# 用法：DRILL_HOME=<私有目录> bash drill3.sh [sid 1000 上正在运行的进程名，先优雅停掉]
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
mark "=== drill 3 (NC-231): a stop hook that ignores its ctx"
RUNNING=${1:-}
if [ -n "$RUNNING" ] && alive "$RUNNING"; then kill -TERM "$(pidof "$RUNNING")"; wait_exit "$RUNNING" 30; fi
for phase in service.stopping service.stopped; do
  name=H-${phase#service.}
  start_game "$name" 1000 config.game.hook.yaml ROOST_DRILL_STUCK_HOOK=$phase
  wait_ready "$name" "$GAME_OPS_PORT" 60 || continue
  mark "SIGTERM $name; $(key 1000)"
  kill -TERM "$(pidof "$name")"
  wait_exit "$name" 30
  mark "after $name: $(key 1000)"
  mark "$name log: hook_entered=$(grep -c 'stop hook entered' "$D/logs/$name.log") shutdown_entered=$(grep -c '"msg":"service shutdown"' "$D/logs/$name.log") mod_stopped=$(grep -c '"msg":"mod stopped' "$D/logs/$name.log") released=$(grep -c '"msg":"singleton: released"' "$D/logs/$name.log") app_run_failed=$(grep -c '"msg":"app run failed"' "$D/logs/$name.log")"
  mark "$name run error: $(python3 "$SCRIPTS/condense.py" "$D/logs/$name.log" '^app run failed$' | cut -c1-400)"
  # 卡住的 stopping 不释放锁：等键过期再起下一个，不让下一个进程把等待时间算进它的停机观测。
  for _ in $(seq 1 40); do [ "$($R exists "$KEY_PREFIX:game:1000")" = 0 ] && break; sleep 0.5; done
done
start_game G3 1000 config.game.yaml
wait_ready G3 "$GAME_OPS_PORT" 60
mark "=== drill 3 done"
