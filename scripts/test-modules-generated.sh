#!/usr/bin/env bash
# 业务模块（roost module）在生成工程上的验收：全新工程、自定义 module 名、安装带依赖的模块、
# 重复安装无变化、模块自带场景数与工程级检查、先装一个再装另一个（增量）、全部模块换顺序一次装完。
# 离线：模块场景用替身提交器，不连 Mongo / NATS / Redis。
set -euo pipefail
repo_root="${ROOST_MODULES_REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
review_root="${ROOST_MODULES_OUTPUT:-$(mktemp -d "${TMPDIR:-/tmp}/roost-modules.XXXXXX")}"
mkdir -p "$review_root"
succeeded=0
verify_index=0
all_modules=(wallet checkin chat enter profile bag leaderboard mailbox task stamina shop skillbook room turn rehome actor scene monster attribute equipment dungeon naming cdkey notice gacha friends idle terrain combat servertime login gm purchase rules team matchroom matchmaking alliance activity activity_world activity_alliance portal)
reverse_modules=()
for ((i=${#all_modules[@]}-1; i>=0; i--)); do reverse_modules+=("${all_modules[i]}"); done
all_counts=(combat+skillbook=1 notice+gm=4 wallet=17 checkin=8 chat=10 enter=29 profile=20 bag=20 leaderboard=21 mailbox=32 task=29 stamina=25 shop=20 skillbook=11 room=22 turn=25 rehome=46 actor=14 scene=28 monster=26 attribute=20 equipment=18 dungeon=21 naming=15 cdkey=12 notice=8 gacha=29 friends=18 idle=19 terrain=28 combat=38 servertime=12 login=32 portal=12 gm=9 purchase=21 purchase+bag=4 purchase+enter=2 wallet+gm=4 bag+gm=4 profile+gm=4 mailbox+gm=4 rehome+gm=4 chat+enter=2 enter+profile=3 mailbox+enter=5 mailbox+bag=9 mailbox+profile=10 task+checkin=3 rehome+enter=5 scene+chat=5 scene+profile=2 scene+attribute=2 scene+equipment=2 attribute+equipment=8 dungeon+leaderboard=3 combat+scene=2 combat+equipment=2 terrain+monster=2 servertime+enter=3 login+enter=6 portal+enter=2 profile+enter=4 attribute+enter=2 task+wallet=6 task+bag=3 task+shop=3 task+stamina=3 task+profile=2 task+dungeon=2 task+leaderboard=3 rules=10 shop+rules=2 stamina+rules=2 team=35 team+rehome=2 matchroom=12 matchmaking=17 matchmaking+rehome=2 alliance=55 alliance+enter=2 alliance+rehome=2 activity=21 activity_world=4 activity_alliance=5 activity+enter=2 activity+profile=1 activity+leaderboard=1 activity+mailbox=1)
cleanup() {
  if [[ -z "${ROOST_MODULES_OUTPUT:-}" && "$succeeded" == 1 && "${ROOST_MODULES_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "Generated consumers and evidence: $review_root"
  fi
}
trap cleanup EXIT
export GOWORK=off
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
roost="$review_root/roost"

# new_project <目录名> <module>：-template game 的工程，依赖指向本检出；提交一次，之后用 git 看有没有改动。
new_project() {
  local dir="$review_root/$1"
  "$roost" project new "$1" -module "$2" -out "$dir" -template game --skip-deps > "$review_root/$1-new.log" 2>&1
  (cd "$dir" && go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root" &&
    git init -q . && git add -A && git -c user.email=modules@example.com -c user.name=modules commit -qm base)
  echo "$dir"
}
commit_all() { git add -A && git -c user.email=modules@example.com -c user.name=modules commit -qm "$1"; }
# expect_verify <期望的 "模块=通过数 ..."}>：verify 的 JSON 里每个模块恰好这么多条通过、零跳过，整体 ok。
expect_verify() {
  verify_index=$((verify_index+1))
  local report="$review_root/verify-$verify_index-$(basename "$PWD").json"
  "$roost" module verify --json > "$report" 2> "$report.stderr" || { cat "$report" "$report.stderr"; exit 1; }
  python3 - "$report" "$@" <<'PY'
import json, sys
report = json.load(open(sys.argv[1]))
want = dict(item.split('=') for item in sys.argv[2:])
got = {m['name']: (m['passed'], m['skipped'], m['ok']) for m in report['modules']}
assert report['ok'], report
assert len(report['modules']) == len(got), report['modules']
assert set(got) == set(want), (got, want)
for name, count in want.items():
    assert got[name] == (int(count), 0, True), (name, got[name], count)
assert all(check['ok'] for check in report['checks']), report['checks']
assert {check['name'] for check in report['checks']} == {'go build ./...', 'go vet ./...', 'roost id check', 'roost generate --check'}, report['checks']
assert len(report['checks']) == 4, report['checks']
print('verify:', ' '.join(f'{name}={got[name][0]}' for name in sorted(got)), '+', len(report['checks']), 'project checks')
PY
}

# 1. 全新工程，自定义 module 名：装 checkin，依赖 wallet 跟着装。
project="$(new_project atelier example.com/studio/atelier)"
cd "$project"
"$roost" module list > "$review_root/list.log"
for name in "${all_modules[@]}"; do
  grep -q "^  $name " "$review_root/list.log" || { cat "$review_root/list.log"; echo "roost module list does not show $name" >&2; exit 1; }
done
"$roost" module plan checkin --json > "$review_root/plan.json"
[[ -z "$(git status --porcelain)" ]] || { git status --short; echo "roost module plan changed the project" >&2; exit 1; }
python3 - "$review_root/plan.json" <<'PY'
import json, sys
plan = json.load(open(sys.argv[1]))
assert not plan['applied'] and [m['name'] for m in plan['modules']] == ['wallet', 'checkin'], plan
assert all(m['status'] == 'install' for m in plan['modules']), plan
ids = [(kind, value) for m in plan['modules'] for kind, names in m['ids'].items() for value in names.values()]
assert len(ids) == len(set(ids)), ids
PY
"$roost" module add checkin > "$review_root/add.log"
grep -q 'example.com/studio/atelier/protocol/pb' game/handler/checkin_claim.go
if grep -rnE '\{\{(ID:[a-z]+:[A-Za-z0-9_]+|MODULE|PROJECT|GAME_SERVICE|GAME_SERVICE_PKG|TIMER_COMPONENT)\}\}' cmd db/def game internal/errors protocol/def; then echo "an installed file still carries a placeholder" >&2; exit 1; fi
commit_all modules
# 2. 重复安装：没有任何变化。
"$roost" module add checkin wallet > "$review_root/add-again.log"
grep -q 'module wallet: already installed' "$review_root/add-again.log"
[[ -z "$(git status --porcelain)" ]] || { git status --short; echo "a repeated install changed the project" >&2; exit 1; }
# 3. 模块自带场景与工程级检查。
expect_verify wallet=17 checkin=8
[[ -z "$(git status --porcelain)" ]] || { git status --short; echo "roost module verify changed the project" >&2; exit 1; }

# 4. 增量：先只装 wallet，之后在已有模块的工程上再装 checkin。
project="$(new_project bazaar example.com/studio/bazaar)"
cd "$project"
"$roost" module add wallet > "$review_root/add-wallet.log"
commit_all wallet
expect_verify wallet=17
"$roost" module add checkin > "$review_root/add-checkin.log"
grep -q 'module wallet: already installed' "$review_root/add-checkin.log"
grep -q 'module checkin: installed' "$review_root/add-checkin.log"
expect_verify wallet=17 checkin=8
# 5. 再叠一个用托管服务、又接管它的生成桩的模块：-template game 不带 chat，安装器在这次安装里声明它；
#    chat 模块的协作者替换 chat 托管服务的拒绝桩，doctor 不再把它当成未实现。
! grep -q '^    chat:' roost.yaml
commit_all checkin
"$roost" module add chat > "$review_root/add-chat.log"
grep -q 'framework service chat: declared' "$review_root/add-chat.log"
grep -q '^        framework: chat' roost.yaml
[[ -f configs/service/config.chat.yaml ]]
grep -q 'replaces the untouched generated stub' "$review_root/add-chat.log"
grep -q 'prerequisite (did run): roost add transport tcp' "$review_root/add-chat.log"
expect_verify wallet=17 checkin=8 chat=10
"$roost" project doctor > "$review_root/doctor.log" 2>&1 || true
grep -q '^OK    collaborators:chat' "$review_root/doctor.log"
# 6. enter：进入游戏端点，并由安装器打开自身视图（玩家 TCP 接入已由 chat 在上一步装好，这里不再重复）；
#    之后 doctor 的 self-sync 为 OK。enter 单独装时自己装 TCP 接入，见第 8 步。
commit_all chat
"$roost" module add enter > "$review_root/add-enter.log"
grep -Fq 'prerequisite (did run): enable access.player.self_sync after module declarations; sync in the same installation transaction' "$review_root/add-enter.log"
! grep -q 'roost add transport tcp' "$review_root/add-enter.log"
# chat 与 enter 都装着了：chat 的联动文件（进入游戏时加进在线名单）在这一步写进工程，场景单独计数。
grep -q 'glue chat with enter: written' "$review_root/add-enter.log"
[[ -f game/chat/chat_enter.go ]]
grep -q 'self_sync: true' roost.yaml
expect_verify wallet=17 checkin=8 chat=10 enter=29 chat+enter=2
go test -mod=mod ./... > "$review_root/test-all.log" 2>&1 || { tail -30 "$review_root/test-all.log"; exit 1; }
"$roost" project doctor > "$review_root/doctor.log" 2>&1 || true
grep -q '^OK    self-sync' "$review_root/doctor.log"
# 工程只托管模块用到的服务（这里只有 chat，协作者已由模块接管）：没有任何 FAIL。
[[ "$(grep -c '^FAIL' "$review_root/doctor.log")" == 0 ]] || { grep '^FAIL' "$review_root/doctor.log"; echo "unexpected doctor failures" >&2; exit 1; }
# 7. 在已有四个模块的工程上再叠第二批：profile、bag、leaderboard（托管 rank）、mailbox（托管 mail，依赖已装的 wallet）。
commit_all enter
"$roost" module add profile bag leaderboard mailbox > "$review_root/add-batch2.log"
grep -q 'module wallet: already installed' "$review_root/add-batch2.log"
grep -q 'glue mailbox with enter: written' "$review_root/add-batch2.log"
# enter 已在：角色名的联动（新建请求的名字按档案的规则校验并写进档案）随 profile 写进工程。
grep -q 'glue enter with profile: written' "$review_root/add-batch2.log"
# bag 与 mailbox 同一次装：道具附件的联动（mailbox 的附件种类登记点）也在这一步写。
grep -q 'glue mailbox with bag: written' "$review_root/add-batch2.log"
expect_verify wallet=17 checkin=8 chat=10 enter=29 chat+enter=2 enter+profile=3 profile=20 bag=20 leaderboard=21 mailbox=32 mailbox+enter=5 mailbox+bag=9 mailbox+profile=10 profile+enter=4
go test -mod=mod ./... > "$review_root/test-all-batch2.log" 2>&1 || { tail -30 "$review_root/test-all-batch2.log"; exit 1; }
"$roost" project doctor > "$review_root/doctor.log" 2>&1 || true
grep -q '^OK    collaborators:mail' "$review_root/doctor.log" && grep -q '^OK    collaborators:rank' "$review_root/doctor.log"
grep -q 'framework service mail: declared' "$review_root/add-batch2.log" && grep -q 'framework service rank: declared' "$review_root/add-batch2.log"
[[ "$(grep -c '^FAIL' "$review_root/doctor.log")" == 0 ]] || { grep '^FAIL' "$review_root/doctor.log"; echo "unexpected doctor failures" >&2; exit 1; }

# 自带 Room 在已有业务工程上增量安装；其 Entity / DAO / lifecycle 与 owner 接线一次完成。
commit_all batch2
"$roost" module add room > "$review_root/add-room.log"
[[ -f game/entities/room/entity.go && -f db/def/room.go && -f game/entities/room/lifecycle_gen.go ]]
expect_verify wallet=17 checkin=8 chat=10 enter=29 chat+enter=2 enter+profile=3 profile=20 bag=20 leaderboard=21 mailbox=32 mailbox+enter=5 mailbox+bag=9 mailbox+profile=10 room=22 profile+enter=4

# A3: reuse the room installed above, and wire its persistent deadline atomically.
commit_all room
"$roost" module add turn > "$review_root/add-turn.log"
expect_verify wallet=17 checkin=8 chat=10 enter=29 chat+enter=2 enter+profile=3 profile=20 bag=20 leaderboard=21 mailbox=32 mailbox+enter=5 mailbox+bag=9 mailbox+profile=10 room=22 turn=25 profile+enter=4

# 8. 全新工程、一条命令、与上面相反的顺序装全部模块：结果与安装顺序无关，重复安装无变化。
project="$(new_project harbor example.com/studio/harbor)"
cd "$project"
"$roost" module add turn room mailbox leaderboard bag profile chat checkin enter > "$review_root/add-all.log"
grep -q 'prerequisite (did run): roost add transport tcp' "$review_root/add-all.log"
grep -Fq 'prerequisite (did run): enable access.player.self_sync after module declarations; sync in the same installation transaction' "$review_root/add-all.log"
grep -q 'self_sync: true' roost.yaml
grep -q 'glue chat with enter: written' "$review_root/add-all.log" && grep -q 'glue mailbox with enter: written' "$review_root/add-all.log"
grep -q 'glue mailbox with bag: written' "$review_root/add-all.log"
# 第三批：task（依赖已装的 wallet）。checkin 已在，签到推进每日任务的联动随它写进工程。
"$roost" module add task stamina shop skillbook > "$review_root/add-task.log"
# rehome（单个玩家换服）：enter 已在，进入前检查 / 到达 / 意图对账的联动随它写进工程。
"$roost" module add rehome > "$review_root/add-rehome.log"
grep -q 'glue rehome with enter: written' "$review_root/add-rehome.log"
# rehome 用 account（needs.uses）：工程还没有，安装器在这一步声明；之后 login 接管的是这一步生成的 account 拒绝桩。
grep -q 'framework service account: declared' "$review_root/add-rehome.log"
# servertime（服务器时间）：不挂实体；enter 已在，进入时推送的联动随它写进工程。
"$roost" module add servertime > "$review_root/add-servertime.log"
grep -q 'glue servertime with enter: written' "$review_root/add-servertime.log"
# login（登录鉴权）：接管玩家 TCP 的鉴权桩与 account 的 collaborators（都还是没改过的生成桩）；
# enter 已在，进入前检查的联动随它写进工程。之后 doctor 不再把这两处当成未实现。
"$roost" module add login > "$review_root/add-login.log"
grep -q 'glue login with enter: written' "$review_root/add-login.log"
"$roost" project doctor -workflow player-tcp > "$review_root/doctor-harbor.log" 2>&1 || true
grep -q '^OK    collaborators:account' "$review_root/doctor-harbor.log"
grep -q '^OK    workflow:tcp-auth' "$review_root/doctor-harbor.log"
[[ -f cmd/accountctl/main.go ]]
# portal（登录入口）：依赖已装的 login；入口挂在托管 account 服务的进程上（清单的 runtime.framework），
# 安装器代办的 web Mod 加在 account 上，game 不因此多出 HTTP 监听；enter 已在，整条链路的联动场景随它写进工程；
# 安装后提示（清单的 next）打印成 todo: 行。
"$roost" module add portal > "$review_root/add-portal.log"
grep -q 'prerequisite (did run): roost add mod web -service account' "$review_root/add-portal.log"
grep -q 'glue portal with enter: written' "$review_root/add-portal.log"
grep -q '^  todo:      ' "$review_root/add-portal.log"
grep -q '^web:' configs/service/config.account.yaml
! grep -q '^web:' configs/service/config.game.yaml
grep -q 'kitmodulehost.NewMod("account"' internal/bootstrap/generated.go
grep -q 'glue task with checkin: written' "$review_root/add-task.log"
grep -q 'module bag: already installed' "$review_root/add-task.log"
if grep -rnE '\{\{(ID:[a-z]+:[A-Za-z0-9_]+|MODULE|PROJECT|GAME_SERVICE|GAME_SERVICE_PKG|TIMER_COMPONENT)\}\}' cmd db/def game internal/errors protocol/def; then echo "an installed file still carries a placeholder" >&2; exit 1; fi
commit_all all
"$roost" module add enter bag mailbox task shop skillbook room turn rehome servertime login portal > "$review_root/add-all-again.log"
[[ -z "$(git status --porcelain)" ]] || { git status --short; echo "a repeated install changed the project" >&2; exit 1; }
expect_verify wallet=17 checkin=8 chat=10 enter=29 chat+enter=2 enter+profile=3 profile=20 bag=20 leaderboard=21 mailbox=32 mailbox+enter=5 mailbox+bag=9 mailbox+profile=10 task=29 task+checkin=3 stamina=25 shop=20 skillbook=11 room=22 turn=25 rehome=46 rehome+enter=5 servertime=12 servertime+enter=3 login=32 portal=12 login+enter=6 portal+enter=2 profile+enter=4 task+wallet=6 task+bag=3 task+shop=3 task+stamina=3 task+profile=2 task+leaderboard=3 rules=10 shop+rules=2 stamina+rules=2
go test -mod=mod ./... > "$review_root/test-all-harbor.log" 2>&1 || { tail -30 "$review_root/test-all-harbor.log"; exit 1; }

# 9. 全批次正常/反向顺序各建一个新工程。计划必须覆盖全部模块和所有联动，且不改工程。
#    计划和验收分别核对固定数量；不能用一次命令退出 0 代替场景数，也不覆盖前面的失败证据。
check_full_plan() {
  python3 - "$1" "${all_counts[@]}" <<'PY'
import json, sys
plan = json.load(open(sys.argv[1]))
expected = {name: int(count) for name, count in (item.split('=') for item in sys.argv[2:])}
modules = {module['name']: module for module in plan['modules']}
glue = {item['module'] + '+' + item['with']: item for item in plan.get('glue', [])}
assert not plan['applied'], plan
assert set(modules) == {name for name in expected if '+' not in name}, modules
assert set(glue) == {name for name in expected if '+' in name}, glue
positions = {module['name']: i for i, module in enumerate(plan['modules'])}
for name, module in modules.items():
    assert module['status'] == 'install', module
    assert module['verify']['pass'] == expected[name], (name, module['verify'])
    assert all(positions[dependency] < positions[name] for dependency in (module.get('requires') or [])), module
for name, item in glue.items():
    if name in expected:
        assert item['verify']['pass'] == expected[name], (name, item)
# timer type ids are numbered per entity (turn on Room and activity on Player both start at 2), every other kind is project-wide.
ids = [(kind if kind != 'timer' else 'timer@' + ((module.get('timer') or {}).get('entity') or ''), value) for module in modules.values() for kind, names in (module.get('ids') or {}).items() for value in names.values()]
assert len(ids) == len(set(ids)), ids
print('plan:', len(modules), 'modules,', len(glue), 'glue, unique IDs and dependency order')
PY
}
check_clean() {
  [[ -z "$(git status --porcelain)" ]] || { git status --short; echo "$1 changed the project" >&2; exit 1; }
}
verify_full_project() {
  local name="$1" pair arg
  shift
  project="$(new_project "$name" "example.com/studio/$name")"
  cd "$project"
  "$roost" module plan "$@" --json > "$review_root/$name-plan.json" 2> "$review_root/$name-plan.stderr"
  check_full_plan "$review_root/$name-plan.json"
  check_clean 'full module plan'
  "$roost" module add "$@" > "$review_root/$name-add.log" 2>&1 || { tail -80 "$review_root/$name-add.log"; exit 1; }
  for pair in chat:enter mailbox:enter mailbox:bag task:checkin task:wallet task:bag task:shop task:stamina task:profile task:dungeon task:leaderboard profile:enter attribute:enter rehome:enter scene:chat scene:profile scene:attribute scene:equipment attribute:equipment dungeon:leaderboard combat:skillbook combat:scene combat:equipment terrain:monster servertime:enter login:enter portal:enter enter:profile; do
    grep -q "glue ${pair%:*} with ${pair#*:}: written" "$review_root/$name-add.log" || { cat "$review_root/$name-add.log"; echo "missing glue $pair" >&2; exit 1; }
  done
  if grep -rnE '\{\{(ID:[a-z]+:[A-Za-z0-9_]+|MODULE|PROJECT|GAME_SERVICE|GAME_SERVICE_PKG|TIMER_COMPONENT)\}\}' cmd db/def game internal/errors protocol/def; then echo "an installed file still carries a placeholder" >&2; exit 1; fi
  commit_all full-modules
  # 每个工程再以相反的请求顺序重复安装，所有模块和联动都应保持原样。
  local repeated=() requested=("$@") index
  for ((index=${#requested[@]}-1; index>=0; index--)); do repeated+=("${requested[index]}"); done
  "$roost" module add "${repeated[@]}" > "$review_root/$name-add-again.log" 2>&1 || { tail -80 "$review_root/$name-add-again.log"; exit 1; }
  for arg in "${all_modules[@]}"; do grep -q "module $arg: already installed" "$review_root/$name-add-again.log"; done
  check_clean 'repeated full installation'
  expect_verify "${all_counts[@]}"
  check_clean 'full module verify'
  go test -mod=mod ./... > "$review_root/$name-test-all.log" 2>&1 || { tail -80 "$review_root/$name-test-all.log"; exit 1; }
  check_clean 'full generated project tests'
}
verify_full_project foundry "${all_modules[@]}"
verify_full_project citadel "${reverse_modules[@]}"

succeeded=1
echo "modules: ok"
