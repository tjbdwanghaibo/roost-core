#!/bin/zsh
S=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/cat3
cd /Users/whb/roost/roost-core/.claude/worktrees/agent-a6a633ed0fdf27401/wiring/physicsroom
echo "before: $(uptime) | $(sysctl -n vm.swapusage) | $(pmset -g | grep -E ' (sleep|displaysleep) ' | tr -s ' ' | tr '\n' ';')" > $S/ugc/run6.load
ROOST_PHYSICS_SOAK_MINUTES=${MIN:-20} ROOST_PHYSICS_SOAK_INTERVAL=15 ROOST_PHYSICS_SOAK_TSV=$S/ugc/${TAG:-soak20}.tsv caffeinate -d -i $S/soak.test -test.run '^TestPhysicsRoomSoak$' -test.v -test.timeout 30m > $S/ugc/${TAG:-soak20}.log 2>&1
echo "exit=$? after: $(uptime)" >> $S/ugc/run6.load
