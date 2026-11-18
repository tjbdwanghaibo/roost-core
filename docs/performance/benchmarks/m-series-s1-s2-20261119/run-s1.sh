#!/bin/zsh
# S1 微基准：M-2 / M-9 / M-18（M-5 = E-1 跳过，引用第三类第一批结论）
set -u
cd /Users/whb/roost/roost-core/.claude/worktrees/agent-a7f9590e34cecf2ab
E=docs/performance/benchmarks/m-series-s1-s2-20261119
export GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0
host() { echo "== $1 $(date '+%F %T')"; sysctl -n vm.loadavg; sysctl vm.swapusage; }
{ host "S1 start"; go version; git rev-parse --short HEAD; } > $E/host-s1.txt
host "M-2 start" >> $E/host-s1.txt
go test ./gameplay/movement -run '^$' -bench MoveBeatAllocations -benchmem -count 6 > $E/m2-move-beat.txt 2>&1
echo "M-2 exit=$?" >> $E/host-s1.txt
host "M-9 start" >> $E/host-s1.txt
go test ./framework/sync/entitysync/policy -run '^$' -bench RelationViewAllianceFlush -benchtime 50x -count 3 > $E/m9-relation-alliance.txt 2>&1
echo "M-9 exit=$?" >> $E/host-s1.txt
host "M-18 start" >> $E/host-s1.txt
go test ./framework/sync/entitysync/policy -run '^$' -bench GroupViewFlush -benchtime 200x -count 3 > $E/m18-group-view.txt 2>&1
echo "M-18 exit=$?" >> $E/host-s1.txt
host "S1 end" >> $E/host-s1.txt
