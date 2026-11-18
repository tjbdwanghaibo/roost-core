#!/bin/zsh
# M-9 补充：同一基准，2000 档把 MaxSubscribersPerSubject 提到 players+16（overlay，仓库文件不动）
set -u
S=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad
cd /Users/whb/roost/roost-core/.claude/worktrees/agent-a7f9590e34cecf2ab
E=docs/performance/benchmarks/m-series-s1-s2-20261119
export GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0
go test -c -overlay $S/m9/ov.json -o $S/m9/policy-sub2016.test ./framework/sync/entitysync/policy || exit 1
{ echo "== M-9b start $(date '+%F %T')"; sysctl -n vm.loadavg; sysctl vm.swapusage; } > $E/host-m9b.txt
cd framework/sync/entitysync/policy
$S/m9/policy-sub2016.test -test.run '^$' -test.bench RelationViewAllianceFlush -test.benchtime 50x -test.count 3 -test.benchmem -test.timeout 10m > $S/m9/m9b-raw.txt 2>&1
echo "exit=$?" >> ../../../../$E/host-m9b.txt
cd ../../../..
{ echo "== M-9b end $(date '+%F %T')"; sysctl -n vm.loadavg; sysctl vm.swapusage; } >> $E/host-m9b.txt
grep -c "WARN" $S/m9/m9b-raw.txt >> $E/host-m9b.txt
grep -v "WARN" $S/m9/m9b-raw.txt > $E/m9b-relation-alliance-sub2016.txt
