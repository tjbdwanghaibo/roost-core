#!/usr/bin/env bash
# 别名合服（D15，把服 1001 并入 1000）的两服真实进程演练（可重复；KNOWN-LIMITS“合服（D15）的验证边界”）。
#
# 2026-10-20 第一次演练的脚本留在证据目录（后移入 Git 历史），这里把它做成可重复的入口，并补上当时没有的两段：
#   - B 冷加载按 sid 分库的文档：v1.47 起 Player 是 dbscope=sid（文档在 <工程>_<sid>.player），A 的角色在合服后由
#     game 1000 冷加载 sidmerge 拷进 <工程>_1000 的那一份；断言背包延续 A 的数量，且 B 里那份的 _version 增长、A 的归档不动；
#   - 独立 Gate：game 1000 换成 Game Ingress 接法（cmd/drillingress，嵌入 TCP 关闭），独立 Gate（cmd/drillgate）用
#     wiring/gate 的 AccountAuthenticator 按角色的承载服路由，原服 1001 的角色经 Gate 进到 1000。
#
# 段落：
#   1. 建数据：`make dev-run`（11 个服务，game 1000 嵌入 TCP），game 1001（deploy/dev/second-game.sh 同样的派生配置），
#      两服登记；u1 在 1000、u2 在 1001、u3 两服各一个角色，各加 10 个道具 1001；停两个 game。
#   2. 合服：sidmove wal-drained 两侧；sidmerge migrate -dry-run（冲突时报告并按“A 的文档让给 B”处置，见下）；migrate 与
#      重跑幂等；accountctl merge-server；误启动 1001 被拒；1000 起来后四个角色登录写入（u2 背包 10 → 15：B 冷加载迁入的
#      文档）；1001 上建角被拒、1000 上成功；B 里迁入文档的 _version 都比 A 的归档高。
#   3. 独立 Gate：停 game 1000，换 Game Ingress + 起 Gate；u2、u3@1001、u1 经 Gate 登录写入。
#   4. 回退：停 Gate 与 game 1000，sidmerge revert，unmerge-server，1001 用新 WAL 目录起来；u2 回到 1001（背包延续）。
#
# 依赖与清理同 scripts/test-demo-login.sh（scripts/testdata/demo-drill/lib.sh）：私有 Mongo 副本集 / NATS JetStream / Redis，
# 自起 etcd；结束时停掉全部进程、删本工程的库 / 流 / 键；证据留在 $DEMO_MERGE_OUT。
#
#   source <私有目录>/roost-dataengine-it/env.sh
#   DEMO_MERGE_OUT=/abs/dir scripts/test-demo-merge.sh
#
# DEMO_MERGE_PORT_SHIFT（缺省 26000）、DEMO_MERGE_ETCD_PORT（缺省 34579）、DEMO_MERGE_PROJECT（缺省 mgd<六位十六进制>）、
# DRILL_KEEP=1。不进 pretag。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo replica-set URI required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?private Redis required}"
: "${DEMO_MERGE_OUT:?evidence directory required (absolute)}"
for tool in etcd jq curl mongosh redis-cli nats; do
	command -v "$tool" >/dev/null || { echo "$tool not found on PATH" >&2; exit 2; }
done
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-demo-merge.XXXXXX")"
project="${DEMO_MERGE_PROJECT:-$(printf 'mgd%06x' $(((RANDOM << 9) ^ RANDOM)))}"
out="$DEMO_MERGE_OUT"
port_shift="${DEMO_MERGE_PORT_SHIFT:-26000}"
etcd_port="${DEMO_MERGE_ETCD_PORT:-34579}"
etcd_pid=""
mkdir -p "$out"
export GOWORK=off GOFLAGS="${GOFLAGS:-} -mod=mod"
# shellcheck source=testdata/demo-drill/lib.sh
source "$repo_dir/scripts/testdata/demo-drill/lib.sh"
trap drill_cleanup EXIT

drill_log "project $project, port shift $port_shift, work dir $work_dir, evidence $out"
drill_check_ports
drill_start_etcd
drill_generate
drill_configure
mkdir -p cmd/mergedrill
sed "s|DRILL_MODULE|example.com/$project|g" "$repo_dir/scripts/testdata/demo-drill/merge.go.txt" >cmd/mergedrill/main.go
go build -o bin/mergedrill ./cmd/mergedrill && go build -o bin/sidmerge ./cmd/sidmerge && go build -o bin/sidmove ./cmd/sidmove || drill_fail "build drill tools"

nats_url="${ROOST_DATAENGINE_IT_NATS_URL%%,*}"
game_a=$(drill_second_game_config 1001)
game_b=configs/service/config.game.yaml
account_prefix=$(awk '/^account:/{a=1;next} a&&/^  key_prefix:/{print $2;exit} a&&/^[^ ]/{a=0}' configs/service/config.account.yaml | tr -d '"')
accountctl() { bin/accountctl -redis "$ROOST_DATAENGINE_IT_REDIS_ADDR" -prefix "$account_prefix" "$@"; }
drill() {
	bin/mergedrill "$1" -state "$out/state.json" -account-nats "$nats_url" -prefix "$project" \
		-endpoints "1000=127.0.0.1:$(drill_port 7000),1001=127.0.0.1:$(drill_port 7001)" "${@:2}" | tee -a "$out/drill.log"
}
mongo_eval() { mongosh --quiet "$ROOST_DATAENGINE_IT_MONGO_URI" --eval "$1"; }
merge_flags=(-from 1001 -into 1000 -from-config "$game_a" -into-config "$game_b")
gate_addr="127.0.0.1:$(drill_port 7100)"

# ---- 1. 建数据 ----
drill_log "phase 1: make dev-run, game 1001, roles and items"
make dev-run >"$out/dev-run.log" 2>&1 || { tail -30 "$out/dev-run.log"; drill_fail "make dev-run"; }
drill_start game1001 bin/app game 1001 "$game_a"
accountctl upsert-server -sid 1001 >/dev/null
drill setup -items 10
mongo_eval "printjson({a: db.getSiblingDB('${project}_1001').player.find({}, {_version: 1}).toArray(), b: db.getSiblingDB('${project}_1000').player.find({}, {_version: 1}).toArray()})" >"$out/players-before-merge.txt"
sh deploy/dev/run.sh stop game >/dev/null
drill_stop game1001

# ---- 2. 合服 ----
drill_log "phase 2: merge 1001 into 1000"
bin/sidmove wal-drained -dir data/wal/dataengine/1000 >"$out/wal-1000.json" || drill_fail "WAL 1000 not drained"
bin/sidmove wal-drained -dir data/wal/dataengine/1001 >"$out/wal-1001.json" || drill_fail "WAL 1001 not drained"
if ! bin/sidmerge migrate "${merge_flags[@]}" -dry-run >"$out/dryrun.json" 2>"$out/dryrun.err"; then
	# 两侧都有的按 sid 分库文档（同一个 _id）：报告里点名，业务处置。本演练按“B 的为准”删掉 A 的那几份（A 的库是归档，
	# 处置前先另存一份），然后再查一次。
	jq -c '.conflicts // .Conflicts // empty' "$out/dryrun.json" >"$out/dryrun-conflicts.json" || true
	drill_log "dry-run refused: $(tail -1 "$out/dryrun.err")"
	jq -r '.. | objects | select(has("collection") and has("_id")) | "\(.collection) \(._id)"' "$out/dryrun.json" 2>/dev/null >"$out/conflicts.txt" || true
	[ -s "$out/conflicts.txt" ] || { cat "$out/dryrun.json"; drill_fail "dry-run refused without naming conflicts"; }
	while read -r collection id; do
		mongo_eval "var c = db.getSiblingDB('${project}_1001').getCollection('${collection#*/}'); var key = EJSON.parse('$id')._id; db.getSiblingDB('${project}_drill_set_aside').getCollection('${collection#*/}').insertOne(c.findOne({_id: key})); c.deleteOne({_id: key});" >>"$out/conflict-resolution.log"
	done <"$out/conflicts.txt"
	bin/sidmerge migrate "${merge_flags[@]}" -dry-run >"$out/dryrun-after-resolution.json" || { cat "$out/dryrun-after-resolution.json"; drill_fail "dry-run still refused"; }
fi
bin/sidmerge migrate "${merge_flags[@]}" -manifest "$out/manifest.json" >"$out/migrate.json" || { cat "$out/migrate.json"; drill_fail "migrate"; }
bin/sidmerge migrate "${merge_flags[@]}" -manifest "$out/manifest-again.json" >"$out/migrate-again.json" || drill_fail "migrate re-run"
jq -e '[.. | objects | select(has("copied")) | .copied] | add == 0' "$out/migrate-again.json" >/dev/null || { cat "$out/migrate-again.json"; drill_fail "the re-run copied again"; }
accountctl merge-server -from 1001 -into 1000 -note "demo merge drill" >"$out/merge.json"
accountctl show-server -sid 1001 | tee "$out/show-1001.json" | grep -q '"merged_into": 1000' || drill_fail "1001 not recorded as merged into 1000"
# 误启动 1001：启动自检拒绝，DataEngine 之前退出。
nohup bin/app game --sid 1001 --config "$game_a" >"$out/game1001-refused.log" 2>&1 &
refused=$!
i=0
while kill -0 $refused 2>/dev/null && [ $i -lt 90 ]; do i=$((i + 1)); sleep 1; done
if kill -0 $refused 2>/dev/null; then kill $refused; drill_fail "game 1001 still running after the merge"; fi
grep -q "merged into 1000" "$out/game1001-refused.log" || { tail -20 "$out/game1001-refused.log"; drill_fail "refusal not logged"; }
drill_log "game 1001 refused to start"
sh deploy/dev/run.sh start game >/dev/null || drill_fail "start game 1000"
drill login -who u1:1000 -expect-serving 1000 -add 1 -expect-count 11
drill login -who u2:1001 -expect-serving 1000 -add 5 -expect-count 15
drill login -who u3:1000 -expect-serving 1000 -add 1 -expect-count 11
drill login -who u3:1001 -expect-serving 1000 -add 2 -expect-count 12
drill create -sid 1001 -expect-refused
drill create -sid 1000
# B 冷加载并写了迁入的文档：A 的每一份（归档）在 B 里都有，且 B 的 _version 更高。
mongo_eval "
const a = db.getSiblingDB('${project}_1001').player, b = db.getSiblingDB('${project}_1000').player;
const rows = a.find({}, {_version: 1}).toArray().map(d => ({_id: d._id, archive: Number(d._version), merged: Number((b.findOne({_id: d._id}, {_version: 1}) || {})._version)}));
printjson(rows);
if (rows.length < 2 || rows.some(r => !(r.merged > r.archive))) { print('COLD-LOAD-CHECK-FAILED'); }
" | tee "$out/players-after-merge.txt"
grep -q COLD-LOAD-CHECK-FAILED "$out/players-after-merge.txt" && drill_fail "B did not write the migrated Player documents"
drill_log "phase 2 done: B cold-loaded and wrote the roles migrated from 1001"

# ---- 3. 独立 Gate ----
drill_log "phase 3: game 1000 behind an independent Gate"
sh deploy/dev/run.sh stop game >/dev/null
drill_ingress_config "$game_b" configs/service/config.game-ingress.yaml
drill_gate_config
drill_start game-ingress bin/drillingress game 1000 configs/service/config.game-ingress.yaml
drill_start gate bin/drillgate gate 9500 configs/service/config.gate.yaml DRILL_GATE_ADDR="$gate_addr" DRILL_GATE_ADVERTISE="$gate_addr"
drill login -who u2:1001 -expect-serving 1000 -add 1 -expect-count 16 -gate "$gate_addr"
drill login -who u3:1001 -expect-serving 1000 -add 1 -expect-count 13 -gate "$gate_addr"
drill login -who u1:1000 -expect-serving 1000 -add 1 -expect-count 12 -gate "$gate_addr"
drill_log "phase 3 done: roles of the merged server entered 1000 through the Gate"

# ---- 4. 回退 ----
drill_log "phase 4: revert"
drill_stop gate
drill_stop game-ingress
bin/sidmove wal-drained -dir data/wal/dataengine/1000 >"$out/wal-1000-revert.json" || drill_fail "WAL 1000 not drained before revert"
bin/sidmerge revert "${merge_flags[@]}" -manifest "$out/manifest.json" >"$out/revert.json" || { cat "$out/revert.json"; drill_fail "revert"; }
accountctl unmerge-server -from 1001 -note "demo merge drill revert" >"$out/unmerge.json"
accountctl upsert-server -sid 1001 -status open >/dev/null
mv data/wal/dataengine/1001 "$work_dir/wal-1001-retired"
sh deploy/dev/run.sh start game >/dev/null || drill_fail "start game 1000 after revert"
drill_start game1001 bin/app game 1001 "$game_a"
drill login -who u2:1001 -expect-serving 1001 -add 1 -expect-count 17
drill login -who u3:1001 -expect-serving 1001 -add 1 -expect-count 14
drill login -who u3:1000 -expect-serving 1000 -add 1 -expect-count 12
drill login -who u1:1000 -expect-serving 1000 -add 1 -expect-count 13
drill_log "PASS demo merge drill (evidence in $out)"
