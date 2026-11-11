#!/usr/bin/env bash
# 业务模块（roost module）在生成工程上的验收：全新工程、自定义 module 名、安装带依赖的模块、
# 重复安装无变化、模块自带场景数与工程级检查、先装一个再装另一个（增量）。
# 离线：模块场景用替身提交器，不连 Mongo / NATS / Redis。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="${ROOST_MODULES_OUTPUT:-$(mktemp -d "${TMPDIR:-/tmp}/roost-modules.XXXXXX")}"
mkdir -p "$review_root"
succeeded=0
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
  "$roost" module verify --json > "$review_root/verify.json" || { cat "$review_root/verify.json"; exit 1; }
  python3 - "$review_root/verify.json" "$@" <<'PY'
import json, sys
report = json.load(open(sys.argv[1]))
want = dict(item.split('=') for item in sys.argv[2:])
got = {m['name']: (m['passed'], m['skipped'], m['ok']) for m in report['modules']}
assert report['ok'], report
assert set(got) == set(want), (got, want)
for name, count in want.items():
    assert got[name] == (int(count), 0, True), (name, got[name], count)
assert all(check['ok'] for check in report['checks']), report['checks']
print('verify:', ' '.join(f'{name}={got[name][0]}' for name in sorted(got)), '+', len(report['checks']), 'project checks')
PY
}

# 1. 全新工程，自定义 module 名：装 checkin，依赖 wallet 跟着装。
project="$(new_project atelier example.com/studio/atelier)"
cd "$project"
"$roost" module list > "$review_root/list.log"
grep -q '^  wallet ' "$review_root/list.log" && grep -q '^  checkin ' "$review_root/list.log" && grep -q '^  chat ' "$review_root/list.log"
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
! grep -rn '{{' db/def game internal/errors protocol/def >/dev/null
commit_all modules
# 2. 重复安装：没有任何变化。
"$roost" module add checkin wallet > "$review_root/add-again.log"
grep -q 'module wallet: already installed' "$review_root/add-again.log"
[[ -z "$(git status --porcelain)" ]] || { git status --short; echo "a repeated install changed the project" >&2; exit 1; }
# 3. 模块自带场景与工程级检查。
expect_verify wallet=8 checkin=6
[[ -z "$(git status --porcelain)" ]] || { git status --short; echo "roost module verify changed the project" >&2; exit 1; }

# 4. 增量：先只装 wallet，之后在已有模块的工程上再装 checkin。
project="$(new_project bazaar example.com/studio/bazaar)"
cd "$project"
"$roost" module add wallet > "$review_root/add-wallet.log"
commit_all wallet
expect_verify wallet=8
"$roost" module add checkin > "$review_root/add-checkin.log"
grep -q 'module wallet: already installed' "$review_root/add-checkin.log"
grep -q 'module checkin: installed' "$review_root/add-checkin.log"
expect_verify wallet=8 checkin=6
# 5. 再叠一个接管生成桩的模块：chat 替换 chat 托管服务的拒绝桩，doctor 不再把它当成未实现。
commit_all checkin
"$roost" module add chat > "$review_root/add-chat.log"
grep -q 'replaces the untouched generated stub' "$review_root/add-chat.log"
expect_verify wallet=8 checkin=6 chat=6
"$roost" project doctor > "$review_root/doctor.log" 2>&1 || true
grep -q '^OK    collaborators:chat' "$review_root/doctor.log"

succeeded=1
echo "modules: ok"
