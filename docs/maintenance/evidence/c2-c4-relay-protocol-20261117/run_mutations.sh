#!/usr/bin/env bash
# 逐条变异：改一处 → 跑对应测试 → 恢复。输出追加到 mutations.txt。
set -u
S=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/c2c4
R=/Users/whb/roost/roost-core/.claude/worktrees/agent-af45c8f4e77a2e105
OUT=$S/mutations.txt
export ROOST_DATAENGINE_IT_REDIS_ADDR=127.0.0.1:17379
: > "$OUT"
m() { # name file cmd...
  local name=$1 file=$2; shift 2
  echo "===== $name ($file)" >> "$OUT"
  python3 "$S/mutate.py" "$R" "$file" "$S/$name.old" "$S/$name.new" "$@" >> "$OUT" 2>&1
}
m r1 service/relay/room.go go test -race -count=1 -run 'TestWelcomeCarriesTheRoomEpoch|TestPresenceReachesOnlyTheHost' ./service/relay/
m r2 service/relay/room.go go test -race -count=1 -run 'TestWelcomeCarriesTheRoomEpoch|TestPresenceReachesOnlyTheHost' ./service/relay/
m m3 client/relay/client.go go test -count=1 ./client/relay/
m m4 client/relay/host.go go test -count=1 ./client/relay/
m m5 client/relay/client.go go test -count=1 ./client/relay/
m m6 client/relay/host.go go test -count=1 ./client/relay/
for t in t1:client.ts t2:client.ts t3:client.ts t4:web.ts t5:compress.ts t6:web.ts; do
  m "${t%%:*}" "client/ts/src/${t##*:}" bash -c "cd $R/client/ts && npx tsc -p tsconfig.test.json && node --test --test-concurrency=1 --test-timeout=30000 dist-test/test/*.test.js 2>&1 | grep -E '^✖|^ℹ (pass|fail)' | sort -u"
done
m cs1 client/dotnet/Roost.Client.Relay/RelayClient.cs dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- "$R"
ROOST_RELAY_TESTS_ONLY=presence m cs2 client/dotnet/Roost.Client.Relay/RelayClient.cs dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- "$R"
ROOST_RELAY_TESTS_ONLY=presence m cs3 client/dotnet/Roost.Client.Relay/RelayClient.cs dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- "$R"
m cs4 client/dotnet/Roost.Client.Relay/RelayCompressor.cs dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- "$R"
dotnet build-server shutdown > /dev/null 2>&1
echo "===== done" >> "$OUT"
