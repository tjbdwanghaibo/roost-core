#!/bin/bash
# 物理房间长稳 8 分钟，同时按时刻取 vmmap --summary（逐区域类型的常驻 / 脏页）与 ps RSS、footprint。
S=<scratch>
W=<worktree>
MIN=${MIN:-8}
echo "before: $(uptime) | $(sysctl -n vm.swapusage)" > "$S/host.txt"
cd "$W/wiring/physicsroom"
ROOST_PHYSICS_SOAK_MINUTES=$MIN ROOST_PHYSICS_SOAK_INTERVAL=15 ROOST_PHYSICS_SOAK_TSV="$S/soak.tsv" \
  "$S/../e3soak.test" -test.run '^TestPhysicsRoomSoak$' -test.v -test.timeout 20m > "$S/soak.log" 2>&1 &
pid=$!
echo "pid $pid" >> "$S/host.txt"
start=$(date +%s)
for at in 45 120 210 300 390 465; do
  while (( $(date +%s) - start < at )); do sleep 1; done
  kill -0 $pid 2>/dev/null || break
  t=$(( $(date +%s) - start ))
  rss=$(ps -o rss= -p $pid)
  fp=$(footprint -p $pid -f bytes 2>/dev/null | grep -E "Footprint:" | head -1)
  echo "t=$t rss_kb=$rss $fp load=$(sysctl -n vm.loadavg)" >> "$S/snapshots.txt"
  vmmap --summary $pid > "$S/vmmap-t$t.txt" 2>&1
done
wait $pid; echo "test exit=$?" >> "$S/host.txt"
echo "after: $(uptime) | $(sysctl -n vm.swapusage)" >> "$S/host.txt"
