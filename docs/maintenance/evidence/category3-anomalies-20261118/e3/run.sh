#!/bin/zsh
# E-3：100/s × 30s × 5 轮，串行；每轮前后记 uptime 与 swap。
set -u
source ~/.roost-it/roost-dataengine-it/env.sh
cd /Users/whb/roost/roost-core/.claude/worktrees/agent-a6a633ed0fdf27401
log=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/cat3/e3/runs.log
for n in ${=ROUNDS:-1 2 3 4 5}; do
  echo "round=$n before: $(uptime) | $(sysctl -n vm.swapusage)" >> $log
  ROOST_PERF_LABEL=gate-drop-$n ROOST_GATE_LOAD_RATE=100 ROOST_GATE_LOAD_SECONDS=30 bash scripts/perf/gate-mixed.sh >> $log 2>&1
  echo "round=$n exit=$? after: $(uptime)" >> $log
done
echo ALLDONE >> $log
