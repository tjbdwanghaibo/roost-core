# shellcheck shell=bash
# game-demo 真实进程演练的公共部分：scripts/test-demo-login.sh 与 scripts/test-demo-merge.sh 都 source 它。
#
# 做的事：生成当前检出的 game-demo（工程名 $project，库 / 流 / Redis 键都按它派生，所以每次运行用一个新名字就不与
# 别人撞），把全部开发配置改指向调用方给的私有依赖、端口整体平移 $port_shift（7000 → 7000+shift，8081、9000–9010、
# 9100–9110 同样），JetStream 流缩到 256 MiB（dataengine.effects.max_bytes 与 saga.stream_max_bytes；隔离集群的
# 存储有限），自起私有 etcd；结束时停掉本次起的全部进程、删本工程的库 / 流 / 键，日志留在 $out/logs。
#
# 调用方先设：repo_dir work_dir project out port_shift etcd_port，并在 trap 里调 drill_cleanup。
# 依赖变量：ROOST_DATAENGINE_IT_MONGO_URI / _NATS_URL / _REDIS_ADDR（scripts/mirror-local.sh up 的 env.sh）。

drill_log() { echo "[drill $(date +%H:%M:%S)] $*"; }
drill_fail() {
	drill_log "FAIL: $*"
	exit 1
}

# drill_port <开发配置里的端口>：平移后的端口。
drill_port() { echo $(($1 + port_shift)); }

# 平移后要用到的端口都得空着：game 1000 / 1001、Gate 的客户端口，入口 8081，etcd 广告口 9000–9010、
# 第二个 game 的 8900，ops 9100–9110、9200（第二个 game）、9300（Gate）。
drill_check_ports() {
	local base port
	for base in 7000 7001 7100 8081 8900 9300 9200 $(seq 9000 9010) $(seq 9100 9110); do
		port=$(drill_port "$base")
		if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
			drill_fail "port $port (dev port $base + shift $port_shift) is in use; set another port shift"
		fi
	done
	for port in "$etcd_port" "$((etcd_port + 1))"; do
		if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
			drill_fail "etcd port $port is in use"
		fi
	done
}

drill_start_etcd() {
	mkdir -p "$work_dir/etcd"
	etcd --name drill --data-dir "$work_dir/etcd/data" \
		--listen-client-urls "http://127.0.0.1:$etcd_port" --advertise-client-urls "http://127.0.0.1:$etcd_port" \
		--listen-peer-urls "http://127.0.0.1:$((etcd_port + 1))" --initial-advertise-peer-urls "http://127.0.0.1:$((etcd_port + 1))" \
		--initial-cluster "drill=http://127.0.0.1:$((etcd_port + 1))" >"$work_dir/etcd/etcd.log" 2>&1 &
	etcd_pid=$!
}

# drill_generate：生成工程、replace 指回本仓、编译 bin/app、bin/loadtest、bin/accountctl，以及独立 Gate 的两个演练程序
# bin/drillgate、bin/drillingress（scripts/testdata/demo-drill/*.go.txt）。
drill_generate() {
	drill_log "generating game-demo $project in $work_dir/$project"
	(cd "$repo_dir" && go run ./codegen/cmd/roost project new "$project" -module "example.com/$project" -out "$work_dir/$project" -template game-demo --skip-deps >"$out/generate.log" 2>&1) ||
		drill_fail "generate (see $out/generate.log)"
	cd "$work_dir/$project"
	printf '\nreplace github.com/tjbdwanghaibo/roost-core => %s\n' "$repo_dir" >>go.mod
	local name
	for name in gate ingress; do
		mkdir -p "cmd/drill$name"
		sed "s|DRILL_MODULE|example.com/$project|g" "$repo_dir/scripts/testdata/demo-drill/$name.go.txt" >"cmd/drill$name/main.go"
	done
	mkdir -p bin
	drill_log "building"
	go build -o bin/app . && go build -o bin/loadtest ./cmd/loadtest && go build -o bin/accountctl ./cmd/accountctl &&
		go build -o bin/drillgate ./cmd/drillgate && go build -o bin/drillingress ./cmd/drillingress ||
		drill_fail "build"
}

# drill_edit <sed 参数...> <文件>：可移植的就地 sed（macOS 与 Linux 的 -i 写法不同）。
drill_edit() {
	local target="${*: -1}"
	sed "${@:1:$#-1}" "$target" >"$target.edit-tmp" && mv "$target.edit-tmp" "$target"
}

# drill_configure：全部开发配置改指向私有依赖、端口平移、流缩小。
drill_configure() {
	local nats_url="${ROOST_DATAENGINE_IT_NATS_URL%%,*}" f base args=()
	for base in 7000 8081 $(seq 9000 9010) $(seq 9100 9110); do
		args+=(-e "s#127\.0\.0\.1:$base\$#127.0.0.1:$(drill_port "$base")#" -e "s#0\.0\.0\.0:$base\$#0.0.0.0:$(drill_port "$base")#")
	done
	for f in configs/service/config.*.yaml; do
		case "$f" in *.prod.example.yaml) continue ;; esac
		drill_edit \
			-e "s#mongodb://127.0.0.1:27017/?replicaSet=rs0#$ROOST_DATAENGINE_IT_MONGO_URI#" \
			-e "s#nats://127.0.0.1:4222#$nats_url#" \
			-e "s#^  addr: 127.0.0.1:6379\$#  addr: $ROOST_DATAENGINE_IT_REDIS_ADDR#" \
			-e "s#endpoints: \[127.0.0.1:2379\]#endpoints: [127.0.0.1:$etcd_port]#" \
			-e 's#max_bytes: 8589934592#max_bytes: 268435456#' \
			"${args[@]}" "$f"
	done
	# 入口的区服目录（portal.servers）在行中间："1000": 127.0.0.1:7000
	drill_edit -e "s#\"1000\": 127\.0\.0\.1:7000#\"1000\": 127.0.0.1:$(drill_port 7000)#" configs/service/config.account.yaml
	if grep -nE '(127\.0\.0\.1|0\.0\.0\.0):(7000|8081|90[0-9][0-9]|91[0-9][0-9])$' configs/service/config.*.yaml | grep -v prod.example; then
		drill_fail "dev ports left unshifted (above)"
	fi
	grep -q "max_bytes: 8589934592" configs/service/config.game.yaml && drill_fail "JetStream limits not lowered"
	return 0
}

# drill_second_game_config <sid>：game <sid> 的配置（与 deploy/dev/second-game.sh 同样的改法：sid、ops 9200、客户端 7001、
# etcd 广告 8900，各加平移），写到 configs/service/config.game<sid>.yaml。
drill_second_game_config() {
	local sid="$1" target="configs/service/config.game$1.yaml"
	sed -e "s/^\(  *\)sid: .*/\1sid: $sid/" -e "s/^sid: .*/sid: $sid/" \
		-e "s#^\(  *\)addr: \(.*\):$(drill_port 9100)\$#\1addr: \2:$(drill_port 9200)#" \
		-e "s#^\(  *\)addr: \(.*\):$(drill_port 7000)\$#\1addr: \2:$(drill_port 7001)#" \
		-e "s#^\(  *\)advertise_addr: \(.*\):$(drill_port 9000)\$#\1advertise_addr: \2:$(drill_port 8900)#" \
		configs/service/config.game.yaml >"$target"
	echo "$target"
}

# drill_ingress_config <源配置> <目标>：同一个 game，关掉嵌入 TCP（Game Ingress 要求），玩家经 Gate 进来。
drill_ingress_config() {
	awk 'BEGIN{p=0} /^player_access:/{p=1} p&&/^  tcp:/{t=1} {if (p&&t&&/^    enabled: true$/) {sub(/true/, "false"); t=0} print} /^[^ #]/&&!/^player_access:/{p=0}' "$1" >"$2"
	grep -A6 '^  tcp:' "$2" | grep -q 'enabled: false' || drill_fail "player_access.tcp.enabled not turned off in $2"
}

# drill_gate_config：Gate 进程的配置，从 activity 的配置取 log / redis / nats / etcd / singleton 几段，ops 9300、sid 9500、
# 单实例锁打开（Gate 接线要求）。
drill_gate_config() {
	local target=configs/service/config.gate.yaml
	awk '/^activity:/{skip=1} /^[a-z_]+:/&&!/^activity:/{skip=0} !skip' configs/service/config.activity.yaml >"$target"
	drill_edit -e "s#^  addr: 127.0.0.1:$(drill_port 9102)\$#  addr: 127.0.0.1:$(drill_port 9300)#" \
		-e "s#^  advertise_addr: .*#  advertise_addr: 127.0.0.1:$(drill_port 7100)#" \
		-e 's#^sid: .*#sid: 9500#' "$target"
	awk '/^singleton:/{s=1} s&&/^  enabled: false$/{sub(/false/, "true"); s=0} {print}' "$target" >"$target.tmp" && mv "$target.tmp" "$target"
	grep -A8 '^singleton:' "$target" | grep -q 'enabled: true' || drill_fail "gate singleton not enabled"
}

# drill_start <名字> <二进制> <服务> <sid> <配置> [环境变量...]：起一个进程、等它的 ops /readyz。
drill_start() {
	local name="$1" bin="$2" service="$3" sid="$4" config="$5" ops i=0
	shift 5
	ops=$(awk '/^ops:/{o=1;next} o&&/^  addr:/{print $2;exit} o&&/^[^ ]/{o=0}' "$config" | tr -d '"')
	mkdir -p .dev
	env "$@" nohup "$bin" "$service" --sid "$sid" --config "$config" >".dev/$name.log" 2>&1 &
	echo $! >".dev/$name.pid"
	while [ "$i" -lt 120 ]; do
		curl -fsS -o /dev/null "http://$ops/readyz" 2>/dev/null && { drill_log "$name ready (pid $(cat ".dev/$name.pid"), ops $ops)"; return 0; }
		kill -0 "$(cat ".dev/$name.pid")" 2>/dev/null || { tail -20 ".dev/$name.log"; drill_fail "$name exited before it was ready"; }
		i=$((i + 1))
		sleep 1
	done
	tail -20 ".dev/$name.log"
	drill_fail "$name not ready in 120s"
}

# drill_stop <名字>：SIGTERM，等至多 150 秒（game 的停机窗口），再 kill -9。
drill_stop() {
	local pid i=0
	[ -f ".dev/$1.pid" ] || return 0
	pid=$(cat ".dev/$1.pid")
	kill "$pid" 2>/dev/null || true
	while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 150 ]; do
		i=$((i + 1))
		sleep 1
	done
	kill -9 "$pid" 2>/dev/null || true
	rm -f ".dev/$1.pid"
	drill_log "stopped $1"
}

# drill_robots <标签> <loadtest 参数...>：跑一轮机器人，报告与输出留在 $out，失败时把失败的机器人列出来。
drill_robots() {
	local label="$1"
	shift
	drill_log "robots $label: $*"
	if ! bin/loadtest -nats-prefix "$project" -account-nats "${ROOST_DATAENGINE_IT_NATS_URL%%,*}" \
		-game-ops "127.0.0.1:$(drill_port 9100)" -report "$out/robots-$label.json" "$@" >"$out/robots-$label.out" 2>&1; then
		grep -E 'scenario failed|^loadtest:' "$out/robots-$label.out" | head -20
		drill_fail "robots $label"
	fi
	drill_log "robots $label passed: $(jq -c '{state, count, stats, p95_ms: .quantiles_ms.p95}' "$out/robots-$label.json" 2>/dev/null || echo see "$out/robots-$label.json")"
}

# drill_cleanup：停掉本次起的全部进程（run.sh 起的与 drill_start 起的），日志留下，删本工程的库 / 流 / 键。
drill_cleanup() {
	set +e
	if [[ "${DRILL_KEEP:-0}" == 1 ]]; then
		echo "DRILL_KEEP=1: everything left running in $work_dir (etcd pid $etcd_pid); stop by hand" >&2
		return
	fi
	if [[ -d "$work_dir/$project" ]]; then
		cd "$work_dir/$project" || return
		local f
		for f in .dev/*.pid; do
			[ -f "$f" ] || continue
			case "$(basename "$f" .pid)" in account | activity | chat | global | mail | match | platform | rank | session | alliance | game) ;;
			*) drill_stop "$(basename "$f" .pid)" ;;
			esac
		done
		sh deploy/dev/run.sh stop >/dev/null 2>&1
		mkdir -p "$out/logs"
		cp .dev/*.log "$out/logs/" 2>/dev/null
		mongosh --quiet "$ROOST_DATAENGINE_IT_MONGO_URI" --eval "for (const d of db.adminCommand({listDatabases: 1, nameOnly: true}).databases) { if (/^$project(_|\$)/.test(d.name)) db.getSiblingDB(d.name).dropDatabase() }" >/dev/null 2>&1
		redis-cli -u "redis://$ROOST_DATAENGINE_IT_REDIS_ADDR" --scan --pattern "*$project*" 2>/dev/null | xargs -n 500 redis-cli -u "redis://$ROOST_DATAENGINE_IT_REDIS_ADDR" del >/dev/null 2>&1
		local stream upper
		upper=$(printf '%s' "$project" | tr '[:lower:]' '[:upper:]')
		for stream in $(nats --server "${ROOST_DATAENGINE_IT_NATS_URL%%,*}" stream ls --names 2>/dev/null | grep -i -e "$project" -e "$upper"); do
			nats --server "${ROOST_DATAENGINE_IT_NATS_URL%%,*}" stream rm -f "$stream" >/dev/null 2>&1
		done
	fi
	[[ -n "${etcd_pid:-}" ]] && kill "$etcd_pid" 2>/dev/null && wait "$etcd_pid" 2>/dev/null
	cd /
	rm -rf "$work_dir"
}
