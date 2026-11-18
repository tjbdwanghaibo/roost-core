#!/usr/bin/env bash
# 换服遗留的两项真实进程演练（2026-11-18）：Player 起服预加载时的准入检查，与 Redis Cluster 上的 account 进程。
#
# 生成一个最小工程（缺省模板 + roost add entity player + roost module add enter rehome gm），把 Player 标成
# preload=startup（roost project sync 装上 dataengine.preload），account 的 Redis 指向 Redis Cluster。然后：
#   1. account 的 key_prefix 不带 hash tag：进程在 Mod 初始化时退出，日志点名 account.key_prefix；
#   2. 带 hash tag 的 account 起来；直接打开 account 存储造三个角色（scripts/testdata/rehome-preload/seed.go.txt）：
#      home（绑在 1000）、moved（已从 1000 换到 1001 并提交）、never（从不启动的 1002 发起、期限 2 秒的在途换服）；
#      在 1000 的库里放 home 与 moved 的 Player 文档（moved 那份就是提交之后、删源档之前崩溃留下的旧档）；
#   3. 起 game 1000：预加载报告 listed=2 loaded=1 skipped=1（准入检查否决旧档、不加载），恢复循环不再列库；旧档补搬到
#      1001 的库后从 1000 删除，home 的文档留着；
#   4. account 进程的清扫（每 30 秒）在期限之后中止 never 的换服：角色仍绑 1002，LastTransfer 未提交、备注以 expired: 开头。
#
# source <私有 mirror-local 环境>/roost-dataengine-it/env.sh   # 需要 Mongo 副本集、NATS、Redis 与 Redis Cluster
# REHOME_PRELOAD_OUT=/abs/evidence bash scripts/test-rehome-preload-cluster.sh
# REHOME_PRELOAD_PORT_SHIFT=43000（7000 / 9000 / 9100 / 9101 平移），REHOME_PRELOAD_ETCD_PORT=34989。
# 依赖私有环境由调用方管理；本脚本只清理本次的工程、库、流与键。不进 pretag，不做性能或跨机结论。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo replica-set URI required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?private Redis required}"
: "${ROOST_MIRROR_LOCAL_REDIS_CLUSTER:?private Redis Cluster required (scripts/mirror-local.sh up)}"
: "${REHOME_PRELOAD_OUT:?absolute evidence directory required}"
for tool in etcd jq curl mongosh redis-cli nats python3; do
	command -v "$tool" >/dev/null || { echo "$tool not found" >&2; exit 2; }
done
[[ "$REHOME_PRELOAD_OUT" == /* ]] || { echo 'REHOME_PRELOAD_OUT must be absolute' >&2; exit 2; }
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-rehome-preload.XXXXXX")"
project="rhp$(printf '%06x' $(((RANDOM << 9) ^ RANDOM)))"
out="$REHOME_PRELOAD_OUT"
port_shift="${REHOME_PRELOAD_PORT_SHIFT:-43000}"
etcd_port="${REHOME_PRELOAD_ETCD_PORT:-34989}"
etcd_pid=""
cluster="$ROOST_MIRROR_LOCAL_REDIS_CLUSTER"
account_prefix="{$project-acct}"
mkdir -p "$out"
git -C "$repo_dir" rev-parse HEAD >"$out/baseline.txt"
export GOWORK=off GOFLAGS="${GOFLAGS:--p=2} -mod=mod"
# shellcheck source=testdata/demo-drill/lib.sh
source "$repo_dir/scripts/testdata/demo-drill/lib.sh"
cleanup() {
	set +e
	# account 的键都在一个 hash tag 下（同一个 slot），经 -c 按 slot 路由删除。
	local node
	for node in ${cluster//,/ }; do
		redis-cli -c -h "${node%:*}" -p "${node##*:}" --scan --pattern "$account_prefix*" 2>/dev/null |
			xargs -n 100 redis-cli -c -h "${node%:*}" -p "${node##*:}" del >/dev/null 2>&1
	done
	drill_cleanup
}
trap cleanup EXIT
for base in 7000 9000 9100 9101; do
	port=$(drill_port "$base")
	! lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1 || drill_fail "port $port is in use; set REHOME_PRELOAD_PORT_SHIFT"
done
for port in "$etcd_port" "$((etcd_port + 1))"; do
	! lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1 || drill_fail "etcd port $port is in use"
done
drill_log "project=$project work=$work_dir evidence=$out"
drill_start_etcd

# ---- 生成工程 ----
go build -C "$repo_dir" -o "$work_dir/roost" ./codegen/cmd/roost
roost() { "$work_dir/roost" "$@"; }
(cd "$work_dir" && roost project new "$project" -module "example.com/$project" -out "$work_dir/$project" --skip-deps) >"$out/generate.log" 2>&1 || drill_fail "generate"
cd "$work_dir/$project"
printf '\nreplace github.com/tjbdwanghaibo/roost-core => %s\n' "$repo_dir" >>go.mod
{ roost add entity player && roost module add enter rehome gm; } >>"$out/generate.log" 2>&1 || drill_fail "install enter rehome gm (see $out/generate.log)"
drill_edit -E 's#^(//roost:entity id=1 entityKind=EntityKindPlayer .*)$#\1 preload=startup#' game/entities/player/entity.go
grep -q '^//roost:entity id=1 .* preload=startup$' game/entities/player/entity.go || drill_fail "Player annotation not changed"
{ roost project sync && roost generate; } >>"$out/generate.log" 2>&1 || drill_fail "project sync / generate (see $out/generate.log)"
grep -q 'NewLocalPreloadMod(registry.LocalPreloadKinds("game")' internal/bootstrap/generated.go || drill_fail "the game bootstrap does not install the preload mod"

# ---- 配置：私有依赖、端口平移、account 走 Cluster ----
python3 -I - "$port_shift" "$ROOST_DATAENGINE_IT_MONGO_URI" "${ROOST_DATAENGINE_IT_NATS_URL%%,*}" "$ROOST_DATAENGINE_IT_REDIS_ADDR" "$cluster" "$etcd_port" "$account_prefix" <<'EOF'
import re, sys
shift, mongo, nats, redis, cluster, etcd, prefix = sys.argv[1:]
nodes = "[" + ", ".join(cluster.split(",")) + "]"
for name in ("game", "account"):
    path = f"configs/service/config.{name}.yaml"
    text = open(path).read()
    for base in (7000, 9000, 9100, 9101):
        text = re.sub(rf"(127\.0\.0\.1|0\.0\.0\.0):{base}\n", lambda m: f"{m.group(1)}:{base + int(shift)}\n", text)
    text = text.replace("mongodb://127.0.0.1:27017/?replicaSet=rs0", mongo).replace("nats://127.0.0.1:4222", nats)
    text = text.replace("endpoints: [127.0.0.1:2379]", f"endpoints: [127.0.0.1:{etcd}]").replace("max_bytes: 8589934592", "max_bytes: 268435456")
    if name == "game":
        text = text.replace("  addr: 127.0.0.1:6379\n", f"  addr: {redis}\n")
    else:
        text = text.replace("  addr: 127.0.0.1:6379\n", f"  addr: 127.0.0.1:6379\n  cluster_addrs: {nodes}\n")
        text, n = re.subn(r"(?m)^  key_prefix: roost:.*:account$", f'  key_prefix: "{prefix}"', text)
        assert n == 1, "account.key_prefix not found"
    open(path, "w").write(text)
EOF
mkdir -p bin cmd/seedrehome .dev
sed "s|DRILL_MODULE|example.com/$project|g" "$repo_dir/scripts/testdata/rehome-preload/seed.go.txt" >cmd/seedrehome/main.go
go build -o bin/app . && go build -o bin/seedrehome ./cmd/seedrehome || drill_fail "build"
seed() { bin/seedrehome -redis-cluster "$cluster" -prefix "$account_prefix" "$@"; }
mongo_eval() { mongosh --quiet "$ROOST_DATAENGINE_IT_MONGO_URI" --eval "$1"; }

# ---- 1. 不带 hash tag 的前缀：Mod 初始化失败 ----
sed "s|key_prefix: \"$account_prefix\"|key_prefix: $project-acct-untagged|" configs/service/config.account.yaml >.dev/config.account.untagged.yaml
bin/app account --sid 1000 --config .dev/config.account.untagged.yaml >.dev/account-untagged.log 2>&1 &
untagged_pid=$!
for i in $(seq 1 40); do kill -0 "$untagged_pid" 2>/dev/null || break; sleep 0.5; done
if kill -0 "$untagged_pid" 2>/dev/null; then
	kill -9 "$untagged_pid"
	wait "$untagged_pid" 2>/dev/null || true
	drill_fail "CLUSTER: account kept running with a key_prefix without a hash tag"
fi
! wait "$untagged_pid" || drill_fail "CLUSTER: account exited 0 with a key_prefix without a hash tag"
grep -q 'account.key_prefix.*requires a non-empty closed first hash tag for Redis Cluster' .dev/account-untagged.log ||
	drill_fail "CLUSTER: the untagged prefix was not refused at Mod init: $(tail -2 .dev/account-untagged.log)"
grep 'app run failed' .dev/account-untagged.log >"$out/account-untagged.log"
drill_log "PASS cluster: an untagged account.key_prefix is refused at Mod init"

# ---- 2. account（Cluster、带 hash tag）与种子 ----
drill_start account bin/app account 1000 configs/service/config.account.yaml
grep -q '"msg":"redis mod: connected".*"cluster":true' .dev/account.log || drill_fail "CLUSTER: account is not on the cluster"
seed -ttl 2 >"$out/seed.json"
home=$(jq -r '.[] | select(.kind == "home").entity_id' "$out/seed.json")
moved=$(jq -r '.[] | select(.kind == "moved").entity_id' "$out/seed.json")
never=$(jq -r '.[] | select(.kind == "never").player_id' "$out/seed.json")
jq -e '.[] | select(.kind == "moved") | .binding.server_id == 1001 and .binding.transfer == null' "$out/seed.json" >/dev/null || drill_fail "moved role not bound to 1001"
jq -e '.[] | select(.kind == "never") | .binding.transfer.from == 1002' "$out/seed.json" >/dev/null || drill_fail "never-started transfer missing"
mongo_eval "db.getSiblingDB('${project}_1000').player.insertMany([{_id:Long('$home'),_schema:1,_version:Long(3),enter:{},rehome:{}},{_id:Long('$moved'),_schema:1,_version:Long(3),enter:{},rehome:{}}])" >/dev/null

# ---- 3. game 1000：预加载的准入检查 ----
drill_start game bin/app game 1000 configs/service/config.game.yaml
for i in $(seq 1 120); do
	docs=$(mongo_eval "print(EJSON.stringify({source:db.getSiblingDB('${project}_1000').player.find({},{_id:1,_version:1}).toArray(),target:db.getSiblingDB('${project}_1001').player.find({},{_id:1,_version:1}).toArray()}))")
	jq -e --argjson m "$moved" '(.source | map(._id) | index($m)) == null and (.target | map(._id) | index($m)) != null' <<<"$docs" >/dev/null && break
	sleep 0.5
done
printf '%s\n' "$docs" >"$out/documents-after-start.json"
jq -e --argjson m "$moved" --argjson h "$home" '(.source | map(._id)) == [$h] and (.target[] | select(._id == $m) | ._version) == 3' <<<"$docs" >/dev/null ||
	drill_fail "PRELOAD: the old copy was not moved to 1001 and dropped on 1000 (home kept): $docs"
report=$(grep '"msg":"dataengine preload mod: preloaded a kind"' .dev/game.log | tail -1)
printf '%s\n' "$report" >"$out/preload-report.json"
jq -e '.listed == 2 and .loaded == 1 and .skipped == 1' <<<"$report" >/dev/null || drill_fail "PRELOAD: want listed=2 loaded=1 skipped=1: $report"
! grep -q 'rehome: checked the players stored on this server' .dev/game.log || drill_fail "PRELOAD: the startup check listed the database instead of using the admitter"
grep 'rehome: ' .dev/game.log >"$out/game-1000-rehome.log" || true
drill_log "PASS preload: the admitter skipped the copy bound elsewhere; it was moved to 1001 and dropped on 1000"

# ---- 4. 源服从不启动的在途换服：account 的清扫在期限后中止 ----
for i in $(seq 1 80); do
	seed -show "$never" >"$out/never-binding.json" 2>/dev/null || true
	jq -e '.transfer == null and .last_transfer.committed == false and (.last_transfer.note | startswith("expired:"))' "$out/never-binding.json" >/dev/null && break
	sleep 1
done
jq -e '.server_id == 1002 and .transfer == null and .last_transfer.committed == false and (.last_transfer.note | startswith("expired:"))' "$out/never-binding.json" >/dev/null ||
	drill_fail "SWEEP: the never-started transfer was not aborted as expired: $(cat "$out/never-binding.json")"
grep -q '"msg":"account: aborted a role transfer past its deadline"' .dev/account.log || drill_fail "SWEEP: the account log does not show the sweep"
grep -E 'aborted a role transfer past its deadline|aborted transfers past their deadline' .dev/account.log >"$out/account-sweep.log"
drill_log "PASS sweep: the account process aborted the transfer from a server that never started"
drill_stop game
drill_stop account
drill_log "PASS rehome preload + cluster drill"
