#!/bin/zsh
# 修前（e7ae5ceb：只改了基准）与修后（f4117a45）同机交替测量。工作目录是 policy 包目录。
B=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/bench
cd /Users/whb/roost/roost-core/.claude/worktrees/agent-aee1e41eebd3463a6/framework/sync/entitysync/policy
host() { echo "== $1 $(date '+%H:%M:%S') $(uptime | sed 's/.*load/load/')" >> $B/host.txt; sysctl vm.swapusage >> $B/host.txt; }
alliance() {
  host "$1 alliance start"
  $B/$2.test -test.run '^$' -test.bench 'RelationViewAlliance(Flush|OverCap)' -test.benchtime 20x -test.count 6 -test.timeout 0 > $B/$1-alliance.txt 2>&1
  host "$1 alliance end exit=$?"
}
guards() {
  host "$1 guards start"
  $B/$2.test -test.run '^$' -test.bench 'RelationViewMemberChange|GroupViewFlush|ClusterFlushRandomWalk$' -test.benchmem -test.count 6 -test.timeout 0 > $B/$1-guards.txt 2>&1
  host "$1 guards end exit=$?"
}
alliance after-1 after
alliance before-2 before
alliance after-2 after
guards before-g before
guards after-g after
echo done >> $B/host.txt
