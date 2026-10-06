#!/usr/bin/env bash
# 演练 2（NC-193 / APP-9）：真实 game 进程的 Service.Init 在最后一步（startActivityPhaseConsumer）失败。
# 注入：把 game 的 durable game-activity-phase 预先建成 DeliverLast，game 的 CreateOrUpdateConsumer（DeliverAll）
# 被服务端拒绝（deliver policy can not be updated）。期望：Init 已启动的部分先收回（service shutdown），Mod 逆序停，
# 单实例锁释放，退出码非 0，日志文件有 app run failed。之后恢复 consumer，再起一次能就绪。
# 用法：DRILL_HOME=<私有目录> bash drill2.sh [sid 1000 上正在运行的进程名，先优雅停掉]
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
STREAM=ROOST_EFFECTS DURABLE=game-activity-phase FILTER=roost.effect.activity.phase_due
mark "=== drill 2 (NC-193): Init fails at its last step"
RUNNING=${1:-}
if [ -n "$RUNNING" ] && alive "$RUNNING"; then kill -TERM "$(pidof "$RUNNING")"; wait_exit "$RUNNING" 30; fi
mark "before: $(key 1000)"
mark "inject: $(bin/drillinject conflict "$ROOST_DATAENGINE_IT_NATS_URL" $STREAM $DURABLE $FILTER 2>&1)"
start_game G1 1000 config.game.yaml
wait_ready G1 "$GAME_OPS_PORT" 60
mark "after G1: $(key 1000)"
mark "G1 log: init=$(grep -c '"msg":"mod init' "$D/logs/G1.log") start=$(grep -c '"msg":"mod start' "$D/logs/G1.log") stopped=$(grep -c '"msg":"mod stopped' "$D/logs/G1.log") stop_failed=$(grep -c '"msg":"mod stop failed' "$D/logs/G1.log") service_shutdown=$(grep -c '"msg":"service shutdown"' "$D/logs/G1.log") released=$(grep -c '"msg":"singleton: released"' "$D/logs/G1.log") app_run_failed=$(grep -c '"msg":"app run failed"' "$D/logs/G1.log")"
mark "restore: $(bin/drillinject restore "$ROOST_DATAENGINE_IT_NATS_URL" $STREAM $DURABLE 2>&1)"
start_game G2 1000 config.game.yaml
wait_ready G2 "$GAME_OPS_PORT" 60
mark "after G2: $(key 1000)"
mark "=== drill 2 done"
