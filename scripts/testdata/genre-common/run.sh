#!/usr/bin/env bash
# 回合 PvP / 合作 Roguelike / 社交经营三类玩法的公共生成机制（R9：玩法 = 装模块 + 只写模块没覆盖的部分）。
# 玩法规则与专属断言在各玩法目录；这里只做：生成工程 → 装模块 → 追加玩法字段 → 拷夹具 → generate → 场景 → 门禁 →
# module verify →（显式开关时）真实档。
#
#   GENRE_MODULES="<模块…>" run.sh <玩法> <第一条命令> [<REAL 开关变量名> <REAL 用例名>]
#
# 模块清单由各玩法脚本（scripts/test-<玩法>-generated.sh）的 genre_modules 数组给出，只写玩法直接用的模块
# （依赖由安装器连带装上：turn 带 room）。房间与玩家的主 DAO、玩家 TCP 接入由模块的 plan 代办，这里不再
# add entity / add dao / add timer，也不往 Player 插 Coins（wallet）。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
genre=$1
first_command=$2
real_env="${3:-}"
real_test="${4:-}"
read -r -a genre_modules <<<"${GENRE_MODULES:?GENRE_MODULES is required (the module list of the genre)}"
review_root="${ROOST_GENRE_OUTPUT:-$(mktemp -d "${TMPDIR:-/tmp}/roost-${genre}.XXXXXX")}"
mkdir -p "$review_root"
project_root="$review_root/project"
module="${ROOST_GENRE_MODULE:-example.com/${genre}-practice}"
modules_sh="$repo_root/scripts/testdata/genre-common/modules.sh"
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
# 先指向本检出：后面 generate 里的 go mod tidy 不去解析可能尚未发布的最低 core 版本，生成物也按本检出编译。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
bash "$modules_sh" add "$review_root/roost" "${genre_modules[@]}" >> "$review_root/generate.log" 2>&1
python3 - "$repo_root" "$genre" "$module" "$first_command" <<'PY'
from pathlib import Path
import sys
repo, genre, module, first = sys.argv[1:]
fixtures = Path(repo) / 'scripts/testdata'
common, own = fixtures / 'genre-common', fixtures / genre
rooms = genre in ('turn-pvp', 'coop-roguelike')

# 玩法自己的状态追加在模块的主 DAO 末尾（DAO 定义是业务文件，访问器由 generate 生成）：
# 房间玩法并入 room 模块的 Room（Q3 A），经营挂在主人的 Player 上。
fields = {
    'turn-pvp': ('room.go', 'RoomDao', '// PvpState 是回合 PvP 的生命、胜负与每人最后一条攻击回执（严格 JSON）；轮到谁、期限、序号归 turn。\n'
                 'PvpState string `store:"pvp_state" dao:"nosync"`'),
    'coop-roguelike': ('room.go', 'RoomDao', '// CoopState 是合作 Roguelike 的投票、路线种子、阶段、领奖事实与每人最后一条回执（严格 JSON）。\n'
                       'CoopState string `store:"coop_state" dao:"nosync"`'),
    'social-sim': ('player.go', 'PlayerDao', '// SocialFarm 是社交经营主人的农场（种植、成熟时刻、帮助、收获与操作者回执，严格 JSON）。\n'
                   'SocialFarm string `store:"social_farm" dao:"nosync"`'),
}
filename, typename, field = fields[genre]
p = Path('db/def') / filename
text = p.read_text()
marker = 'type ' + typename + ' struct {'
assert marker in text, p
end = text.index('\n}', text.index(marker))
p.write_text(text[:end] + '\n\n' + field + text[end:])

# 回合 PvP 扩展 turn 组件（turn README：超时要做别的事改 OnDeadline）：超时扣行动方生命、分出胜负时一并结束。
if genre == 'turn-pvp':
    p = Path('game/components/turn/turn.go')
    text = p.read_text()
    for old, new in [
        ('\tstate.SetTimeouts(state.GetTimeouts() + 1)\n',
         '\tstate.SetTimeouts(state.GetTimeouts() + 1)\n\tpvpTimeout(component.owner.Dao()) // 回合 PvP：超时的行动者扣 1 点生命（pvp.go）\n'),
        ('if dao.GetClosed() || present < 2 || state.GetRound() >= state.GetMaxTurns() {',
         'if dao.GetClosed() || present < 2 || state.GetRound() >= state.GetMaxTurns() || pvpOver(dao) { // 回合 PvP：分出胜负即结束（pvp.go）'),
    ]:
        assert text.count(old) == 1, old
        text = text.replace(old, new)
    p.write_text(text)

copies = [
    (fixtures / 'robot-common/snapshot_test.go.txt', 'game/handler/robot_snapshot_test.go'),
    (common / 'protocol.go.txt', 'protocol/def/scenario.go'),
    (common / 'network.go.txt', 'game/modules/scenarionet/dispatch.go'),
    (common / 'robot.go.txt', 'loadtest/scenario.go'),
    (common / 'network_test.go.txt', 'game/handler/network_test.go'),
    (common / 'gameplay_test.go.txt', 'game/handler/scenario_test.go'),
    (own / 'rules.go.txt', 'game/modules/scenariorules/rules.go'),
    (own / 'handlers.go.txt', 'game/handler/scenario.go'),
    (own / 'fixture_test.go.txt', 'game/handler/scenario_genre_test.go'),
    (own / 'scenarios_test.go.txt', 'game/handler/genre_test.go'),
]
if rooms:
    copies += [
        (common / 'room_command.go.txt', 'game/modules/scenarionet/command.go'),
        (common / 'room_test.go.txt', 'game/handler/scenario_room_test.go'),
        (own / 'room_roster.go.txt', 'game/entities/room/scenario.go'),
    ]
else:
    copies.append((own / 'command.go.txt', 'game/modules/scenarionet/command.go'))
if genre == 'turn-pvp':
    copies.append((own / 'turn_hooks.go.txt', 'game/components/turn/pvp.go'))
if genre in ('turn-pvp', 'social-sim'):
    copies += [
        (fixtures / 'robot-common/main.go.txt', 'cmd/robot/main.go'),
        (common / 'robot_README.md', 'loadtest/README.md'),
        (own / 'robot_complete_test.go.txt', 'game/handler/robot_complete_test.go'),
    ]
if genre == 'coop-roguelike':
    copies += [(own / source, target) for source, target in [
        ('robot_main.go.txt', 'cmd/robot/main.go'), ('view_handlers.go.txt', 'game/handler/coop_view_handlers.go'),
        ('view_protocol.go.txt', 'protocol/def/coop_view.go'), ('view_network.go.txt', 'game/modules/scenarionet/coop_view.go'),
        ('robot_full.go.txt', 'loadtest/coop.go'), ('robot_full_test.go.txt', 'game/handler/coop_robot_test.go')]]
# 两个离散玩法共享真实 Runtime 接线；业务崩溃点与恢复断言仍各自维护。默认档也编译 / vet 它们。
if (own / 'persistence/recovery_test.go.txt').exists():
    copies += [(common / 'persistence/runtime_test.go.txt', 'game/handler/session_real_runtime_test.go'),
               (own / 'persistence/recovery_test.go.txt', 'game/handler/session_real_recovery_test.go')]
for source, target in copies:
    p = Path(target)
    # 只允许覆盖模板自带的 Robot 入口与说明；覆盖到模块或生成文件说明路径撞了。
    assert not p.exists() or target in ('cmd/robot/main.go', 'loadtest/README.md'), 'fixture would overwrite a generated or module file: ' + target
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(source.read_text().replace('GENRE_MODULE', module).replace('FIRST_COMMAND', '"' + first + '"'))
if genre in ('turn-pvp', 'social-sim'):
    p = Path('game/handler/network_test.go')
    text = p.read_text().rstrip()
    p.write_text(text[:-1] + '\ncompleteScenarioRobot(t,f,server.Addr().String())\n}\n')
PY
gofmt_dirs=(db/def game/components game/modules/scenariorules game/modules/scenarionet game/handler loadtest protocol/def cmd/robot)
if [[ -d game/entities/room ]]; then gofmt_dirs+=(game/entities/room); fi
gofmt -w "${gofmt_dirs[@]}"
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestScenario' ./game/handler | tee "$review_root/test.log"
passed="$(grep -c '^--- PASS: TestScenario' "$review_root/test.log" || true)"
expected=9
if [[ "$genre" == coop-roguelike || "$genre" == turn-pvp ]]; then expected=10; fi
if [[ "$passed" != "$expected" ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "Expected $expected passing scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod ./...
go vet -mod=mod ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
# 已装模块自带的场景照跑：每个模块通过数 == 本检出 modules/<名>/module.yaml 的 verify.pass、0 skip（结果 module-verify.json）。
bash "$modules_sh" verify "$review_root/roost" "$repo_root" "$review_root/module-verify.json" "${genre_modules[@]}" | tee "$review_root/module-verify.txt"
if [[ -n "$real_env" && "${!real_env:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo replica set required}"
  [[ -n "$real_test" ]] || { echo "REAL test name required" >&2; exit 1; }
  go test -mod=mod -race -count=1 -timeout 3m -v -run "^${real_test}$" ./game/handler | tee "$review_root/real.log"
  passed=$(grep -c "^--- PASS: $real_test " "$review_root/real.log" || true)
  if [[ "$passed" != 1 ]] || grep -q -- '--- SKIP:' "$review_root/real.log"; then
    echo "Expected one real crash suite without SKIP, got $passed" >&2
    exit 1
  fi
fi
succeeded=1
