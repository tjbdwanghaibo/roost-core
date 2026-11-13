#!/usr/bin/env bash
# Shared generation mechanics; gameplay and assertions remain in each genre fixture.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
genre=$1
first_command=$2
review_root="${ROOST_GENRE_OUTPUT:-$(mktemp -d "${TMPDIR:-/tmp}/roost-${genre}.XXXXXX")}"
mkdir -p "$review_root"
project_root="$review_root/project"
module="${ROOST_GENRE_MODULE:-example.com/${genre}-practice}"
succeeded=0
cleanup() {
  if [[ -z "${ROOST_GENRE_OUTPUT:-}" && "$succeeded" == 1 && "${ROOST_GENRE_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "Generated consumer and evidence: $review_root"
  fi
}
trap cleanup EXIT
export GOWORK=off
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new "$genre-practice" -module "$module" -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
for name in Player GameRoom; do
  if [[ "$name" != Player ]]; then "$review_root/roost" add entity "$name" >> "$review_root/generate.log" 2>&1; fi
  "$review_root/roost" add dao "$name" --entity "$name" >> "$review_root/generate.log" 2>&1
done
python3 - "$repo_root" "$genre" "$module" "$first_command" <<'PY'
from pathlib import Path
import sys
repo, genre, module, first = sys.argv[1:]
for filename, typename, fields in [('player','Player','Coins int64 `store:"coins" dao:"persist,sync"`'),('game_room','GameRoom','State string `store:"state" dao:"nosync"`')]:
    p=Path('db/def')/(filename+'.go'); text=p.read_text();marker='type '+typename+'Dao struct {'
    assert marker in text
    p.write_text(text.replace(marker,marker+'\n'+fields+'\n'))
p=Path('game/entities/game_room/entity.go');text=p.read_text();assert 'category=entity.EntityCategoryOther' in text
p.write_text(text.replace('category=entity.EntityCategoryOther','category=entity.EntityCategoryWorld'))
fixtures=Path(repo)/'scripts/testdata'
for source,target in [(fixtures/'robot-common/snapshot_test.go.txt','game/handler/robot_snapshot_test.go'),(fixtures/'genre-common/robot_main.go.txt','cmd/robot/main.go'),(fixtures/'genre-common/protocol.go.txt','protocol/def/scenario.go'),(fixtures/'genre-common/network.go.txt','game/scenarionet/dispatch.go'),(fixtures/'genre-common/robot.go.txt','loadtest/scenario.go'),(fixtures/'genre-common/network_test.go.txt','game/handler/network_test.go'),(fixtures/genre/'rules.go.txt','game/rules/rules.go'),(fixtures/'genre-common/handlers.go.txt','game/handler/scenario.go'),(fixtures/'genre-common/gameplay_test.go.txt','game/handler/scenario_test.go'),(fixtures/genre/'scenarios_test.go.txt','game/handler/genre_test.go')]:
    p=Path(target);p.parent.mkdir(parents=True,exist_ok=True)
    p.write_text(source.read_text().replace('GENRE_MODULE',module).replace('FIRST_COMMAND','"'+first+'"'))
if genre in ('turn-pvp','social-sim'):
    Path('cmd/robot/main.go').write_text((fixtures/'robot-common/main.go.txt').read_text().replace('GENRE_MODULE',module))
    Path('loadtest/README.md').write_text((fixtures/'genre-common/robot_README.md').read_text())
    p=Path('game/handler/robot_complete_test.go')
    p.write_text((fixtures/genre/'robot_complete_test.go.txt').read_text().replace('GENRE_MODULE',module))
    p=Path('game/handler/network_test.go')
    text=p.read_text().rstrip()
    p.write_text(text[:-1]+'\ncompleteScenarioRobot(t,f,server.Addr().String())\n}\n')
if genre == 'coop-roguelike':
    for source,target in [('robot_main.go.txt','cmd/robot/main.go'),('view_handlers.go.txt','game/handler/coop_view_handlers.go'),('view_protocol.go.txt','protocol/def/coop_view.go'),('view_network.go.txt','game/scenarionet/coop_view.go'),('robot_full.go.txt','loadtest/coop.go'),('robot_full_test.go.txt','game/handler/coop_robot_test.go')]:
        p=Path(target);p.parent.mkdir(parents=True,exist_ok=True)
        p.write_text((fixtures/genre/source).read_text().replace('GENRE_MODULE',module))
PY
gofmt -w db/def game/entities/game_room game/rules game/handler game/scenarionet loadtest protocol/def cmd/robot
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestScenario' ./game/handler | tee "$review_root/test.log"
passed="$(grep -c '^--- PASS: TestScenario' "$review_root/test.log" || true)"
expected=9
if [[ "$genre" == coop-roguelike ]]; then expected=10; fi
if [[ "$passed" != "$expected" ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "Expected $expected passing scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod ./...
go vet -mod=mod ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
succeeded=1
