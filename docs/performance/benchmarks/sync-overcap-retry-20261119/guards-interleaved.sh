#!/bin/zsh
# 回归守卫基准：修前 / 修后逐次交替（各 count 1 × 6 轮），抵消负载漂移。
B=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/bench
cd /Users/whb/roost/roost-core/.claude/worktrees/agent-aee1e41eebd3463a6/framework/sync/entitysync/policy
: > $B/before-gi.txt; : > $B/after-gi.txt
for i in 1 2 3 4 5 6; do
  echo "round $i $(date '+%H:%M:%S') $(uptime | sed 's/.*load/load/')" >> $B/host-gi.txt
  for side in before after; do
    $B/$side.test -test.run '^$' -test.bench 'RelationViewMemberChange|GroupViewFlush|ClusterFlushRandomWalk$' -test.benchmem -test.count 1 -test.timeout 0 >> $B/$side-gi.txt 2>&1
  done
done
echo "done $(date '+%H:%M:%S') $(uptime | sed 's/.*load/load/')" >> $B/host-gi.txt
