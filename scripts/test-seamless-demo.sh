#!/usr/bin/env bash
# 分区场景（scene.partition，原型，分支 feat/seamless-world-prototype）在生成 game-demo 上的多进程端到端演练。
#
# 生成工程（-template game-demo，当前检出），十个 demo 进程（九个托管服务 + sid 1000 的 game）之外再起三个 game：
#   sid 1000：分区场景的 Base（不托管格），p1、p2 的连接在这里；
#   sid 1001：格 (0,0) 的 Cell，同时是 p3 的 Base；
#   sid 1002：格 (1,0) 的 Cell（演练中被 kill -9 再重启）；
#   sid 1003：没有分区配置，普通场景（p4）；
#   sid 1004：只做 Base（不托管格），p5 的连接在这里（演练中被 kill -9 再重启：进场回到保存的位置）。
# 依赖全部是调用方给的私有实例（Mongo 副本集、NATS JetStream、Redis），etcd 由本脚本在私有端口自起；不读共享环境。
# 客户端是 scripts/testdata/seamless/drill.go.txt（经真实 account 登录、玩家 TCP、demo 的同步帧解码）。
# 结束时停掉本脚本起的全部进程、删除本次的库；证据（drill 输出、evidence.json、各进程日志）留在 $ROOST_SEAMLESS_OUT。
#
#   ROOST_DATAENGINE_IT_MONGO_URI=… ROOST_DATAENGINE_IT_NATS_URL=… ROOST_DATAENGINE_IT_REDIS_ADDR=… \
#   ROOST_SEAMLESS_ETCD_PORT=58379 ROOST_SEAMLESS_OUT=/abs/dir scripts/test-seamless-demo.sh
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo replica-set URI required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?private Redis required}"
: "${ROOST_SEAMLESS_OUT:?evidence directory required (absolute)}"
etcd_port="${ROOST_SEAMLESS_ETCD_PORT:-58379}"
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-seamless.XXXXXX")"
project="$work_dir/cwdemo"
out="$ROOST_SEAMLESS_OUT"
mkdir -p "$out"
export GOWORK=off
nats_url="${ROOST_DATAENGINE_IT_NATS_URL%%,*}"
extra_sids="1001 1002 1003 1004"

# 固定端口（demo 生成配置）都要空着：别人的 demo 正在用时不跑。
for port in 7000 7001 7002 7003 7004 9000 9100 9101 9102 9103 9104 9105 9106 9107 9108 9109 9201 9202 9203 9204 8901 8902 8903 8904 "$etcd_port" "$((etcd_port + 1))"; do
	if nc -z 127.0.0.1 "$port" 2>/dev/null; then
		echo "port $port is in use; refusing to run" >&2
		exit 2
	fi
done

etcd_pid=""
cleanup() {
	set +e
	if [[ "${ROOST_SEAMLESS_KEEP:-0}" == 1 ]]; then
		echo "seamless: ROOST_SEAMLESS_KEEP=1, everything left running in $work_dir (etcd pid $etcd_pid); stop by hand" >&2
		return
	fi
	if [[ -d "$project" ]]; then
		cd "$project"
		for sid in $extra_sids; do
			[[ -f ".dev/game$sid.pid" ]] && kill "$(cat ".dev/game$sid.pid")" 2>/dev/null
		done
		sleep 3
		for sid in $extra_sids; do
			[[ -f ".dev/game$sid.pid" ]] && kill -9 "$(cat ".dev/game$sid.pid")" 2>/dev/null
		done
		sh deploy/dev/run.sh stop >/dev/null 2>&1
		mkdir -p "$out/logs"
		cp -R .dev "$out/logs/dev" 2>/dev/null
		cp -R log "$out/logs/log" 2>/dev/null
		mongosh --quiet "$ROOST_DATAENGINE_IT_MONGO_URI" --eval 'for (const name of ["cwdemo", "cwdemo_saga"]) { db.getSiblingDB(name).dropDatabase() }' >/dev/null 2>&1
	fi
	[[ -n "$etcd_pid" ]] && kill "$etcd_pid" 2>/dev/null && wait "$etcd_pid" 2>/dev/null
	cd /
	rm -rf "$work_dir"
}
trap cleanup EXIT

echo "seamless: work dir $work_dir, evidence $out"
mkdir -p "$work_dir/etcd"
etcd --name seamless --data-dir "$work_dir/etcd/data" \
	--listen-client-urls "http://127.0.0.1:$etcd_port" --advertise-client-urls "http://127.0.0.1:$etcd_port" \
	--listen-peer-urls "http://127.0.0.1:$((etcd_port + 1))" --initial-advertise-peer-urls "http://127.0.0.1:$((etcd_port + 1))" \
	--initial-cluster "seamless=http://127.0.0.1:$((etcd_port + 1))" >"$work_dir/etcd/etcd.log" 2>&1 &
etcd_pid=$!

cd "$repo_dir"
go run ./codegen/cmd/roost project new cwdemo -module example.com/cwdemo -out "$project" -template game-demo --skip-deps >"$out/generate.log" 2>&1
cd "$project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_dir"
go mod tidy >/dev/null 2>&1 || true
mkdir -p bin
go build -mod=mod -o bin/app .
sed "s|MMO_MODULE|example.com/cwdemo|g" "$repo_dir/scripts/testdata/seamless/drill.go.txt" >"$work_dir/drill.go"
go build -mod=mod -o "$work_dir/drill" "$work_dir/drill.go"

# 全部配置指向私有依赖；JetStream 流缩到 64MB；单实例锁缩短（kill -9 后的重启只等 ttl）。
for f in configs/service/config.*.yaml; do
	case "$f" in *.prod.example.yaml) continue ;; esac
	sed -i '' \
		-e "s#mongodb://127.0.0.1:27017/?replicaSet=rs0#$ROOST_DATAENGINE_IT_MONGO_URI#" \
		-e "s#nats://127.0.0.1:4222#$nats_url#" \
		-e "s#^  addr: 127.0.0.1:6379#  addr: $ROOST_DATAENGINE_IT_REDIS_ADDR#" \
		-e "s#endpoints: \[127.0.0.1:2379\]#endpoints: [127.0.0.1:$etcd_port]#" \
		-e 's#max_bytes: 8589934592#max_bytes: 67108864#' \
		-e 's#^  ttl: 15s#  ttl: 6s#' -e 's#^  renew_interval: 3s#  renew_interval: 1s#' \
		-e 's#^  guard: 5s#  guard: 2s#' -e 's#^  startup_wait: 30s#  startup_wait: 10s#' "$f"
done
sed -i '' 's#game_sids: \[1000, 1001, 1003\]#game_sids: [1000, 1001, 1002, 1003, 1004]#' configs/activity_groups.yaml
grep -q 'game_sids: \[1000, 1001, 1002, 1003, 1004\]' configs/activity_groups.yaml

# 分区场景：2 × 1 个 3000 × 3000 的格；格 (0,0) 归 sid 1001、格 (1,0) 归 sid 1002。四组怪：分界线两侧各三只静止的
# （视野连续与跨界攻击），一只沿 X 往返的（约每 40s 过一次分界线：Monster 跨界）。回写 1s：崩溃前的 HP 很快进快照。
cp configs/service/config.game.yaml "$work_dir/config.game.ordinary.yaml"
cat >>configs/service/config.game.yaml <<'YAML'
scene:
  partition:
    enabled: true
    world: cwdemo
    cell_size: 3000
    cols: 2
    rows: 1
    owners: ["1001", "1002"]
    ghost_band: 500
    hysteresis: 50
    view_radius: 400
    idle_period: 1s
    writeback_every: 1s
    lease_timeout: 3s
    max_speed: 300
    attack_damage: 10
    spawn_x: 2900
    spawn_y: 1500
    player_hp: 100
    spawns:
      near_a: {x: 2960, y: 1500, count: 3, spread: 15}
      near_b: {x: 3010, y: 1500, count: 3, spread: 15}
      walker: {x: 1500, y: 1700, count: 1, vx: 150}
YAML

derive() { # sid ops client advertise source
	sed -e "s/^\(  *\)sid: .*/\1sid: $1/" -e "s/^sid: .*/sid: $1/" \
		-e "s#^\(  *\)addr: \(.*\):9100\$#\1addr: \2:$2#" \
		-e "s#^\(  *\)addr: \(.*\):7000\$#\1addr: \2:$3#" \
		-e "s#^\(  *\)advertise_addr: \(.*\):9000\$#\1advertise_addr: \2:$4#" \
		"$5" >"configs/service/config.game$1.yaml"
}
derive 1001 9201 7001 8901 configs/service/config.game.yaml
derive 1002 9202 7002 8902 configs/service/config.game.yaml
derive 1004 9204 7004 8904 configs/service/config.game.yaml
# sid 1003 没有 scene.partition：普通场景。
derive 1003 9203 7003 8903 "$work_dir/config.game.ordinary.yaml"

READY_TIMEOUT=120 sh deploy/dev/run.sh start >"$out/run-start.log" 2>&1 || { tail -40 "$out/run-start.log"; exit 1; }
prefix="$(awk '/^account:/{a=1;next} a&&/^  key_prefix:/{print $2;exit} a&&/^[^ ]/{a=0}' configs/service/config.account.yaml | tr -d '"')"
for sid in 1001 1003 1004; do
	go run -mod=mod ./cmd/accountctl -redis "$ROOST_DATAENGINE_IT_REDIS_ADDR" -prefix "${prefix:-roost:account}" upsert-server -sid "$sid" >/dev/null
done

start_game() { # sid ops
	./bin/app game --sid "$1" --config "configs/service/config.game$1.yaml" >>".dev/game$1.log" 2>&1 &
	echo $! >".dev/game$1.pid"
	local i=0
	until curl -fsS -o /dev/null "http://127.0.0.1:$2/readyz" 2>/dev/null; do
		kill -0 "$(cat ".dev/game$1.pid")" 2>/dev/null || { echo "game $1 exited" >&2; tail -30 ".dev/game$1.log" >&2; return 1; }
		i=$((i + 1))
		[[ "$i" -lt 240 ]] || { echo "game $1 not ready" >&2; tail -30 ".dev/game$1.log" >&2; return 1; }
		sleep 0.5
	done
}
start_game 1001 9201
start_game 1002 9202
start_game 1003 9203
start_game 1004 9204

cat >"$work_dir/restart-1002.sh" <<EOF
cd "$project"
i=0
while kill -0 \$(cat .dev/game1002.pid) 2>/dev/null; do i=\$((i+1)); [ \$i -lt 50 ] || exit 1; sleep 0.1; done
./bin/app game --sid 1002 --config configs/service/config.game1002.yaml >>.dev/game1002.log 2>&1 &
echo \$! > .dev/game1002.pid
i=0
until curl -fsS -o /dev/null http://127.0.0.1:9202/readyz 2>/dev/null; do i=\$((i+1)); [ \$i -lt 240 ] || exit 1; sleep 0.5; done
EOF
cat >"$work_dir/restart-1004.sh" <<EOF
cd "$project"
i=0
while kill -0 \$(cat .dev/game1004.pid) 2>/dev/null; do i=\$((i+1)); [ \$i -lt 50 ] || exit 1; sleep 0.1; done
./bin/app game --sid 1004 --config configs/service/config.game1004.yaml >>.dev/game1004.log 2>&1 &
echo \$! > .dev/game1004.pid
i=0
until curl -fsS -o /dev/null http://127.0.0.1:9204/readyz 2>/dev/null; do i=\$((i+1)); [ \$i -lt 240 ] || exit 1; sleep 0.5; done
EOF
set +e
"$work_dir/drill" -account-nats "$nats_url" -prefix cwdemo \
	-endpoints 1000=127.0.0.1:7000,1001=127.0.0.1:7001,1003=127.0.0.1:7003,1004=127.0.0.1:7004 \
	-ops 1000=127.0.0.1:9100,1001=127.0.0.1:9201,1002=127.0.0.1:9202,1003=127.0.0.1:9203,1004=127.0.0.1:9204 \
	-kill "kill -9 \$(cat $project/.dev/game1002.pid)" -restart "sh $work_dir/restart-1002.sh" \
	-kill-base "kill -9 \$(cat $project/.dev/game1004.pid)" -restart-base "sh $work_dir/restart-1004.sh" \
	-out "$out/evidence.json" 2>&1 | tee "$out/drill.log"
status=${PIPESTATUS[0]}
set -e
grep -h "partition:" .dev/game*.log | head -40 >"$out/partition-log-lines.txt" || true
if [[ "$status" != 0 ]]; then
	echo "seamless: drill failed (status $status); logs in $out" >&2
	exit "$status"
fi
for name in enter own_state straight jitter diagonal attack_across_0 attack_across_1 monster_cross cell_crash base_restart ordinary_scene; do
	grep -q "^STEP $name ok" "$out/drill.log" || { echo "seamless: step $name did not pass" >&2; exit 1; }
done
# 普通场景不回归：demo 自己的机器人（cmd/loadtest，完整链路）在没有分区配置的 sid 1003 上跑一轮。
if ! go run -mod=mod ./cmd/loadtest -endpoint 127.0.0.1:7003 -server-id 1003 -count 2 -account-nats "$nats_url" -nats-prefix cwdemo \
	-report "$out/loadtest-1003.json" >"$out/loadtest-1003.log" 2>&1; then
	echo "seamless: the demo robots failed on the ordinary scene (sid 1003); see $out/loadtest-1003.log" >&2
	tail -20 "$out/loadtest-1003.log" >&2
	exit 1
fi
echo "seamless: all steps passed; evidence in $out"
