#!/usr/bin/env bash
# 演练收尾：停掉 DRILL_HOME 下起的全部 bin/app（先 SIGCONT，被 SIGSTOP 的进程收不到 TERM）、etcd，再
# mirror-local clean（停私有依赖、删数据目录）。用法：DRILL_HOME=<私有目录> CORE_DIR=<roost-core> bash teardown.sh
set -uo pipefail
: "${DRILL_HOME:?set DRILL_HOME}" "${CORE_DIR:?set CORE_DIR}"
D=$DRILL_HOME
for f in "$D"/logs/*.pid "$D"/logs/dev/*.pid; do
  [ -f "$f" ] || continue
  pid=$(cat "$f")
  if ps -o command= -p "$pid" 2>/dev/null | grep -q 'bin/app'; then kill -CONT "$pid" 2>/dev/null; kill -TERM "$pid" 2>/dev/null; fi
done
sleep 3
for f in "$D"/logs/*.pid "$D"/logs/dev/*.pid; do
  [ -f "$f" ] || continue
  pid=$(cat "$f")
  if ps -o command= -p "$pid" 2>/dev/null | grep -q 'bin/app'; then kill -KILL "$pid" 2>/dev/null; echo "killed $pid"; fi
done
if [ -f "$D/etcd/pid" ] && ps -o command= -p "$(cat "$D/etcd/pid")" 2>/dev/null | grep -q etcd; then kill "$(cat "$D/etcd/pid")"; fi
ROOST_MIRROR_LOCAL_HOME="$D/env" ROOST_MIRROR_LOCAL_OFFSET="${DRILL_OFFSET:-30000}" bash "$CORE_DIR/scripts/mirror-local.sh" clean
ps -axo pid=,command= | grep -F "$D/" | grep -v grep || echo "no process left under $D"
