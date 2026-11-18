#!/usr/bin/env bash
# 在生成工程里对 room_presence.go 做变异，跑修后的夹具，记录结果，然后恢复。
set -uo pipefail
base=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/rvfix
proj=$base/green
src=$proj/game/modules/room/room_presence.go
cp "$src" "$base/room_presence.go.keep"
export GOWORK=off
run() {
  local name=$1 from=$2 to=$3
  cp "$base/room_presence.go.keep" "$src"
  python3 - "$src" "$from" "$to" <<'PY'
import sys
p, old, new = sys.argv[1:]
s = open(p).read()
assert s.count(old) == 1, (old, s.count(old))
open(p, "w").write(s.replace(old, new))
PY
  (cd "$proj" && go test -count=1 -v -run '^TestRoomPresenceRetriesAFailedDisconnectAndGivesUp$' ./game/modules/room/ > "$base/rv03-mutation-$name.log" 2>&1)
  echo "$name exit=$?"
  grep -E "^(--- |ok|FAIL)|room_presence_test" "$base/rv03-mutation-$name.log"
}
# 1. 去掉重试：第一次失败就放弃
run no-retry 'if current.failures >= maxDisconnectFailures {' 'if true { // MUTATION'
# 2. 少一次重试（两次失败就放弃）
run give-up-after-2 'const maxDisconnectFailures = 3' 'const maxDisconnectFailures = 2 // MUTATION'
# 3. 不放弃（无限重试）
run never-give-up 'if current.failures >= maxDisconnectFailures {' 'if false { // MUTATION'
cp "$base/room_presence.go.keep" "$src"
grep -c MUTATION "$src" || true
