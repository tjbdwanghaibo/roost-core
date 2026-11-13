#!/usr/bin/env bash
# CellWorld: generated game-demo acceptance with private dependencies and real clients.
#
# 生成工程（-template game-demo，当前检出），十一个 demo 进程（九个托管服务、alliance 与 sid 1000 的 game）之外再起四个 game：
#   sid 1000：分区场景的 Base（不托管格），p1、p2 的连接在这里；
#   sid 1001：格 (0,0) 的 Cell，同时是 p3 的 Base；
#   sid 1002：格 (1,0) 的 Cell（演练中被 kill -9 再重启）；
#   sid 1003：没有分区配置，普通场景（p4）；
#   sid 1004：只做 Base（不托管格），p5、p6 的连接在这里（演练中两次被 kill -9 再重启：p5 进场回到保存的位置；p6 与
#             sid 1000 上的 p1 组队，kill 后 3 个快照周期从 p1 的队伍面板移除、重启后恢复）。
# 依赖全部是调用方给的私有实例（Mongo 副本集、NATS JetStream、Redis），etcd 由本脚本在私有端口自起；不读共享环境。
# 客户端是 scripts/testdata/seamless/drill.go.txt（经真实 account 登录、玩家 TCP、demo 的同步帧解码）。
# 结束时停掉本脚本起的全部进程、删除本次的库；证据（drill 输出、evidence.json、各进程日志）留在 $ROOST_SEAMLESS_OUT。
#
#   ROOST_DATAENGINE_IT_MONGO_URI=… ROOST_DATAENGINE_IT_NATS_URL=… ROOST_DATAENGINE_IT_REDIS_ADDR=… \
#   ROOST_SEAMLESS_ETCD_PORT=58379 ROOST_SEAMLESS_OUT=/abs/dir scripts/test-seamless-demo.sh
#
# ROOST_SEAMLESS_TERRAIN=1：开地形的分区演练（可编辑地形 T05 / T06 / T08 的真实多进程验收，客户端步骤在
# scripts/testdata/seamless/terrain.go.txt）。分区换成 2 × 1 个 12800（两块地形）见方的格；主视野是游戏尺度（P5：Base 用
# TerrainView，离开上限 67.2m），所以 view_radius 6800、hysteresis 200、ghost_band 7000 = view_radius + hysteresis；所有
# game 进程配同一个 4 × 2 块的平地地形包（演练程序写出，70m 的带让每个格装下整张图）；起 sid 1001（格 A）、
# 1002（格 B）、1004（只做 Base），不起普通场景 sid 1003、不跑它的机器人。不设（缺省）时是原来的演练，行为不变。
# ROOST_SEAMLESS_PROJECT_HOOK：生成工程之后、编译之前在工程目录里执行的一条 shell 命令（变异验证用：例如删掉装地面
# 钩子的那一行，确认演练会变红）。
# Optional Robot load after the fault drill: ROOST_SEAMLESS_ROBOTS=1000,
# ROOST_SEAMLESS_ROBOT_SECONDS=900, ROOST_SEAMLESS_ROBOT_ACTIVE=5.
# ROOST_SEAMLESS_ROBOT_ONLY=1 skips the fault drill; useful for isolated capacity measurements.
# ROOST_SEAMLESS_CAPACITY=1 installs 9000 NPCs and enables a 30s Robot spread warmup; requires ROBOT_ONLY=1.
set -euo pipefail
# Portable in-place sed: macOS and Linux use different -i spellings.
edit_file() {
 local target="${@: -1}"
 local temporary="$target.edit-tmp"
 sed "${@:1:$#-1}" "$target" >"$temporary"
 mv "$temporary" "$target"
}
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
terrain="${ROOST_SEAMLESS_TERRAIN:-0}"
capacity="${ROOST_SEAMLESS_CAPACITY:-0}"
world="cwdemo-$$"
if [[ "$capacity" == 1 && ( "$terrain" == 1 || "${ROOST_SEAMLESS_ROBOT_ONLY:-0}" != 1 ) ]]; then
 echo "capacity mode requires ROBOT_ONLY=1 and no terrain drill" >&2
 exit 2
fi
extra_sids="1001 1002 1003 1004"
[[ "$terrain" == 1 ]] && extra_sids="1001 1002 1004"

# 固定端口（demo 生成配置）都要空着：别人的 demo 正在用时不跑。
for port in 7000 7001 7002 7003 7004 9000 9010 9100 9101 9102 9103 9104 9105 9106 9107 9108 9109 9110 9201 9202 9203 9204 8901 8902 8903 8904 "$etcd_port" "$((etcd_port + 1))"; do
	if nc -z 127.0.0.1 "$port" 2>/dev/null; then
		echo "port $port is in use; refusing to run" >&2
		exit 2
	fi
done

etcd_pid=""
sampler_pid=""
cleanup() {
	set +e
	if [[ -n "$sampler_pid" ]]; then
		kill "$sampler_pid" 2>/dev/null
		wait "$sampler_pid" 2>/dev/null
	fi
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
		mongosh --quiet "$ROOST_DATAENGINE_IT_MONGO_URI" --eval 'for (const name of ["cwdemo", "cwdemo_saga", "cwdemo_remote"]) { db.getSiblingDB(name).dropDatabase() }' >/dev/null 2>&1
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
go mod tidy >"$out/tidy.log" 2>&1
if [[ -n "${ROOST_SEAMLESS_PROJECT_HOOK:-}" ]]; then
	echo "seamless: project hook: $ROOST_SEAMLESS_PROJECT_HOOK"
	sh -c "$ROOST_SEAMLESS_PROJECT_HOOK"
fi
mkdir -p bin
go build -mod=mod -o bin/app .
mkdir -p "$work_dir/drillsrc"
for f in drill terrain; do
	sed "s|MMO_MODULE|example.com/cwdemo|g" "$repo_dir/scripts/testdata/seamless/$f.go.txt" >"$work_dir/drillsrc/$f.go"
done
go build -mod=mod -o "$work_dir/drill" "$work_dir/drillsrc/drill.go" "$work_dir/drillsrc/terrain.go"

# 全部配置指向私有依赖；JetStream 流缩到 64MB；单实例锁缩短（kill -9 后的重启只等 ttl）。
for f in configs/service/config.*.yaml; do
	case "$f" in *.prod.example.yaml) continue ;; esac
	edit_file \
		-e "s#mongodb://127.0.0.1:27017/?replicaSet=rs0#$ROOST_DATAENGINE_IT_MONGO_URI#" \
		-e "s#nats://127.0.0.1:4222#$nats_url#" \
		-e "s#^  addr: 127.0.0.1:6379#  addr: $ROOST_DATAENGINE_IT_REDIS_ADDR#" \
		-e "s#endpoints: \[127.0.0.1:2379\]#endpoints: [127.0.0.1:$etcd_port]#" \
		-e 's#max_bytes: 8589934592#max_bytes: 67108864#' \
		-e 's#^  ttl: 15s#  ttl: 6s#' -e 's#^  renew_interval: 3s#  renew_interval: 1s#' \
		-e 's#^  guard: 5s#  guard: 2s#' -e 's#^  startup_wait: 30s#  startup_wait: 10s#' "$f"
done
edit_file -E 's#game_sids: \[[0-9, ]*\]#game_sids: [1000, 1001, 1002, 1003, 1004]#' configs/activity_groups.yaml
grep -q 'game_sids: \[1000, 1001, 1002, 1003, 1004\]' configs/activity_groups.yaml

cp configs/service/config.game.yaml "$work_dir/config.game.ordinary.yaml"
if [[ "$terrain" == 1 ]]; then
	# 开地形的分区场景：4 × 2 块（256m × 128m）的平地包，2 × 1 个 12800 的格（每格 2 × 2 块，块不跨格），世界正好是地图。
	# 游戏尺度的主视野（P5）：view_radius 6800 ≥ 离开上限 6720，ghost_band 7000 = view_radius 6800 + hysteresis 200（越过
	# 分界线 200 才交接，演练在这 200 里放邻格的坎）；idle_period 0：
	# 怪一直 50ms 一拍（演练按 GM 观察它的折回点）。格 A 里一只沿 y = 9000 往返的怪；玩家进场点 (11800, 3000) 在格 A。
	mkdir -p configs/terrain
	"$work_dir/drill" -write-flat-pack configs/terrain/drill-flat.pack
	cat >>configs/service/config.game.yaml <<'YAML'
scene:
  terrain:
    pack: configs/terrain/drill-flat.pack
  partition:
    enabled: true
    world: cwdemo
    cell_size: 12800
    cols: 2
    rows: 1
    owners: ["1001", "1002"]
    ghost_band: 7000
    hysteresis: 200
    view_radius: 6800
    idle_period: 0s
    writeback_every: 1s
    lease_timeout: 3s
    max_speed: 600
    attack_damage: 10
    spawn_x: 11800
    spawn_y: 3000
    player_hp: 100
    spawns:
      walker: {x: 6000, y: 9000, count: 1, vx: 600}
YAML
else
# 分区场景：2 × 1 个 3000 × 3000 的格；格 (0,0) 归 sid 1001、格 (1,0) 归 sid 1002。四组怪：分界线两侧各三只静止的
# （视野连续与跨界攻击），一只沿 X 往返的（约每 40s 过一次分界线：Monster 跨界）。回写 1s：崩溃前的 HP 很快进快照。
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
fi
if [[ "$capacity" == 1 ]]; then
 # Two 3000-square Cells, 90 rows of 100 NPCs. The active percentage applies to each row.
 active="${ROOST_SEAMLESS_ROBOT_ACTIVE:-5}"
 [[ "$active" -ge 1 && "$active" -le 100 ]] || { echo "active percentage must be in 1..100" >&2; exit 2; }
 # Replace the drill's spawn section rather than adding duplicate YAML keys.
 python3 - "$active" "${ROOST_SEAMLESS_ROBOTS:-0}" <<'PYCONFIG'
from pathlib import Path
import sys
active=int(sys.argv[1])
robots=int(sys.argv[2])
if not 1<=robots<=10000: raise SystemExit('capacity robots must be in 1..10000')
p=Path('configs/service/config.game.yaml')
s=p.read_text()
# All robots originate on this machine. Keep production admission defaults unchanged.
s=s.replace('    max_connections_per_ip: 128',f'    max_connections_per_ip: {max(128,robots)}',1)
start=s.index('    spawns:',s.index('  partition:'))
s=s[:start].replace('    idle_period: 1s','    idle_period: 0s')+'    spawns:\n'
for row in range(90):
 # A coprime stride distributes exactly active/100 moving NPCs over both Cells.
 moving=[(col*37+row*17)%100<active for col in range(100)]
 start=0
 while start<100:
  end=start+1
  while end<100 and moving[end]==moving[start]: end+=1
  vx=(150 if row%2==0 else -150) if moving[start] else 0
  s+=f'      load_{row:03d}_{start:03d}: {{x: {30+60*start}, y: {16+33*row}, count: {end-start}, spread: 60, vx: {vx}}}\n'
  start=end
p.write_text(s)
PYCONFIG
fi

# Each run has its own durable ownership/birth namespace; restarts inside this run keep it.
edit_file -e "s/^    world: cwdemo$/    world: $world/" configs/service/config.game.yaml

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
# sid 1003 没有 scene.partition：普通场景（开地形的演练不起它）。
[[ "$terrain" == 1 ]] || derive 1003 9203 7003 8903 "$work_dir/config.game.ordinary.yaml"

READY_TIMEOUT=120 sh deploy/dev/run.sh start >"$out/run-start.log" 2>&1 || { tail -40 "$out/run-start.log"; exit 1; }
prefix="$(awk '/^account:/{a=1;next} a&&/^  key_prefix:/{print $2;exit} a&&/^[^ ]/{a=0}' configs/service/config.account.yaml | tr -d '"')"
account_sids="1001 1003 1004"
[[ "$terrain" == 1 ]] && account_sids="1001 1002 1004" # 开地形的演练有玩家连格 B（改格 B 的块）
for sid in $account_sids; do
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
[[ "$terrain" == 1 ]] || start_game 1003 9203
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
drill_mode=()
endpoints=1000=127.0.0.1:7000,1001=127.0.0.1:7001,1003=127.0.0.1:7003,1004=127.0.0.1:7004
if [[ "$terrain" == 1 ]]; then
	drill_mode=(-terrain)
	endpoints=1000=127.0.0.1:7000,1001=127.0.0.1:7001,1002=127.0.0.1:7002,1004=127.0.0.1:7004
fi
if [[ "${ROOST_SEAMLESS_ROBOT_ONLY:-0}" != 1 ]]; then
set +e
"$work_dir/drill" ${drill_mode[@]+"${drill_mode[@]}"} -account-nats "$nats_url" -prefix cwdemo \
	-endpoints "$endpoints" \
	-ops 1000=127.0.0.1:9100,1001=127.0.0.1:9201,1002=127.0.0.1:9202,1003=127.0.0.1:9203,1004=127.0.0.1:9204 \
	-kill "kill -9 \$(cat $project/.dev/game1002.pid)" -restart "sh $work_dir/restart-1002.sh" \
	-kill-base "kill -9 \$(cat $project/.dev/game1004.pid)" -restart-base "sh $work_dir/restart-1004.sh" \
	-out "$out/evidence.json" 2>&1 | tee "$out/drill.log"
status=${PIPESTATUS[0]}
set -e
grep -h "partition:" .dev/game*.log | head -40 >"$out/partition-log-lines.txt" || true
grep -h "terrain" .dev/game*.log | head -80 >"$out/terrain-log-lines.txt" || true
if [[ "$status" != 0 ]]; then
	echo "seamless: drill failed (status $status); logs in $out" >&2
	exit "$status"
fi
if [[ "$terrain" == 1 ]]; then
	for name in terrain_start terrain_ghost_edit terrain_player_step terrain_monster_step terrain_cell_restart terrain_consistent; do
		grep -q "^STEP $name ok" "$out/drill.log" || { echo "seamless: step $name did not pass" >&2; exit 1; }
	done
	echo "seamless: all terrain steps passed; evidence in $out"
	exit 0
fi
for name in enter own_state straight jitter diagonal attack_across_0 attack_across_1 panel_level_up death_reward_respawn monster_cross cell_crash base_restart team_panel team_roster_cross_base ordinary_scene; do
	grep -q "^STEP $name ok" "$out/drill.log" || { echo "seamless: step $name did not pass" >&2; exit 1; }
done
# 普通场景不回归：demo 自己的机器人（cmd/loadtest，完整链路）在没有分区配置的 sid 1003 上跑一轮。
# 阈值用 loadtest 缺省的 16s（按一两个游戏进程定：最慢的机器人 7～10s 的两倍）。本演练起 5 个游戏进程；
# saga 中写 Player 的步骤按发起方的 sid 确切路由（维护者 2026-11-11 方案 ③），只投给属主进程的 durable，
# 不再在另外 4 个进程上各拒一次、按 nak 退避重投（11-10 时每步约 +3.75s、合计约 22s，p95 27.9～30.4s，
# 当时临时放宽到 40s）。路由后实测 p95 5.0s，见 docs/maintenance/evidence/saga-step-sid-route-20261111/README.md。
if ! go run -mod=mod ./cmd/loadtest -endpoint 127.0.0.1:7003 -server-id 1003 -count 2 -account-nats "$nats_url" -nats-prefix cwdemo \
	-report "$out/loadtest-1003.json" >"$out/loadtest-1003.log" 2>&1; then
	echo "seamless: the demo robots failed on the ordinary scene (sid 1003); see $out/loadtest-1003.log" >&2
	tail -20 "$out/loadtest-1003.log" >&2
	exit 1
fi
fi
robots="${ROOST_SEAMLESS_ROBOTS:-0}"
if [[ "$robots" -gt 0 ]]; then
 seconds="${ROOST_SEAMLESS_ROBOT_SECONDS:-60}"
 active="${ROOST_SEAMLESS_ROBOT_ACTIVE:-5}"
 warmup=0s
 radius=0
 if [[ "$capacity" == 1 ]]; then warmup=30s; radius=150; fi
 # Keep compilation outside the measurement. The two snapshots include actor counts and cumulative failures.
 go build -mod=mod -o "$work_dir/loadtest" ./cmd/loadtest
 snapshot() {
  local phase="$1"
  for port in 9100 9201 9202; do
   curl -fsS --max-time 10 -H 'Content-Type: application/json' -H 'X-Admin-Token: dev-gm-token' \
    -d '{"name":"gm.scene.partition"}' "http://127.0.0.1:$port/admin/execute" >"$out/partition-$port-$phase.json"
   curl -fsS --max-time 10 "http://127.0.0.1:$port/metrics" >"$out/metrics-$port-$phase.txt"
  done
  python3 - "$out" "$phase" <<'PYSNAPSHOT'
import json,sys
from pathlib import Path
for port in (9100,9201,9202):
 p=Path(sys.argv[1])/f'partition-{port}-{sys.argv[2]}.json'
 value=json.loads(p.read_text())
 if not value.get('ok') or 'node' not in value.get('data',{}):
  raise SystemExit(f'invalid partition snapshot: {p}')
PYSNAPSHOT
 }
 snapshot before
 python3 - "$out" <<'PYPIDS'
import json,sys
from pathlib import Path
mapping={p.read_text().strip():p.stem for p in Path('.dev').glob('*.pid')}
(Path(sys.argv[1])/'service-pids.json').write_text(json.dumps(mapping,indent=2)+'\n')
PYPIDS
 pids="$(cat .dev/*.pid | awk 'BEGIN{ORS=""} {if(NR>1)printf ","; printf "%s",$1}')"
 (while :; do date +%s; ps -p "$pids" -o pid=,time=,rss=; sleep 1; done) >"$out/resources.txt" 2>&1 &
 sampler_pid=$!
 set +e
 "$work_dir/loadtest" -scenario cellworld -endpoint 127.0.0.1:7000 -server-id 1000 \
  -count "$robots" -account-nats "$nats_url" -nats-prefix cwdemo -ramp-step 20 -ramp-interval 100ms \
  -cellworld-duration "${seconds}s" -cellworld-active "$active" -cellworld-warmup "$warmup" -cellworld-view-radius "$radius" -max-p95 "$((seconds + 120))" \
  -report "$out/robot.json" >"$out/robot.log" 2>&1
 robot_status=$?
 set -e
 kill "$sampler_pid" 2>/dev/null || true
 wait "$sampler_pid" 2>/dev/null || true
 sampler_pid=""
 snapshot after
 if [[ "$robot_status" != 0 ]]; then
  echo "seamless: cellworld robots failed; see $out/robot.log" >&2
  tail -10 "$out/robot.log" >&2
  exit "$robot_status"
 fi
fi
echo "seamless: all steps passed; evidence in $out"
