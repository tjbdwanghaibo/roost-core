#!/usr/bin/env bash
# test-demo-login.sh 的真实模块阶段；可用 DEMO_LOGIN_MODULES_ONLY=1 单独复跑。
# 每次生成 casual + naming，所有进程共用本次唯一前缀；不修改公共 lib.sh。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?private Redis required}"
: "${DEMO_LOGIN_OUT:?absolute evidence directory required}"
[[ "$DEMO_LOGIN_OUT" = /* ]] || { echo 'DEMO_LOGIN_OUT must be absolute' >&2; exit 2; }
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-login-modules.XXXXXX")"
project="$(printf 'lgm%06x' $(((RANDOM << 9) ^ RANDOM)))"
out="$DEMO_LOGIN_OUT"
port_shift="${DEMO_LOGIN_PORT_SHIFT:-30000}"
etcd_port="${DEMO_LOGIN_ETCD_PORT:-34479}"
etcd_pid=""
mkdir -p "$out"
export GOWORK=off GOFLAGS="${GOFLAGS:--p=2} -mod=mod"
source "$repo_dir/scripts/testdata/demo-drill/lib.sh"
probe_pids=()
login_cleanup() {
	# macOS Bash 3 在 nounset 下展开空数组会报错；清理必须继续执行。
	set +u
	local pid
	for pid in "${probe_pids[@]}"; do kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; done
	drill_cleanup
}
trap login_cleanup EXIT
{ go version; git -C "$repo_dir" rev-parse HEAD; printf 'project=%s port_shift=%s etcd_port=%s\n' "$project" "$port_shift" "$etcd_port"; } >"$out/env.txt"
drill_check_ports
for base in 7101 8082 9301 9401; do
	lsof -nP -iTCP:"$(drill_port "$base")" -sTCP:LISTEN >/dev/null 2>&1 && drill_fail "extra port $base occupied"
done
drill_start_etcd
drill_log "casual + naming project $project, evidence $out"
(cd "$repo_dir" && go build -o "$out/roost" ./codegen/cmd/roost)
"$out/roost" project new "$project" -module "example.com/$project" -recipe casual -out "$work_dir/$project" --skip-deps >"$out/generate.log" 2>&1
cd "$work_dir/$project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_dir"
"$out/roost" module add naming >>"$out/generate.log" 2>&1
"$out/roost" project sync --skip-deps >>"$out/generate.log" 2>&1
"$out/roost" generate >>"$out/generate.log" 2>&1
go mod tidy >"$out/dependencies.log" 2>&1
for name in gate ingress login-client; do
	mkdir -p "cmd/drill$name"
	sed "s|DRILL_MODULE|example.com/$project|g" "$repo_dir/scripts/testdata/demo-drill/$name.go.txt" >"cmd/drill$name/main.go"
done
gofmt -w cmd/drill*/main.go
"$out/roost" id check >"$out/id-check.log" 2>&1
"$out/roost" generate --check >"$out/generate-check.log" 2>&1
go build ./... >"$out/build.log" 2>&1
go vet ./... >"$out/vet.log" 2>&1
mkdir -p bin
go build -o bin/app .
go build -o bin/accountctl ./cmd/accountctl
go build -o bin/drillgate ./cmd/drillgate
go build -o bin/drillingress ./cmd/drillingress
go build -race -o bin/drilllogin ./cmd/drilllogin-client
drill_configure
# 第二个 account 同一服务、不同进程，与 A 共享正式 Redis 前缀及 NATS queue group。
sed -e "s/:$(drill_port 9101)\$/:$(drill_port 9401)/" -e "s/:$(drill_port 8081)\$/:$(drill_port 8082)/" \
	configs/service/config.account.yaml >configs/service/config.account2.yaml
drill_start account bin/app account 1000 configs/service/config.account.yaml
drill_start account2 bin/app account 1001 configs/service/config.account2.yaml
drill_start mail bin/app mail 1000 configs/service/config.mail.yaml
drill_start game bin/app game 1000 configs/service/config.game.yaml
drill_second_game_config 1001 >/dev/null
drill_start game1001 bin/app game 1001 configs/service/config.game1001.yaml
prefix="$(awk '/^account:/{a=1;next} a&&/^  key_prefix:/{print $2;exit}' configs/service/config.account.yaml)"
for sid in 1000 1001; do
	bin/accountctl -redis "$ROOST_DATAENGINE_IT_REDIS_ADDR" -prefix "$prefix" upsert-server -sid "$sid" >>"$out/register.log" 2>&1
done
# 第二服也通过真实 portal 选服；静态地址目录是应用配置，不由夹具替换认证器。
for f in configs/service/config.account.yaml configs/service/config.account2.yaml; do
	printf '    "1001": 127.0.0.1:%s\n' "$(drill_port 7001)" >>"$f"
done
drill_stop account; drill_stop account2
drill_start account bin/app account 1000 configs/service/config.account.yaml
drill_start account2 bin/app account 1001 configs/service/config.account2.yaml
portal_a="http://127.0.0.1:$(drill_port 8081)"
portal_b="http://127.0.0.1:$(drill_port 8082)"
probe() {
	local label="$1"; shift
	bin/drilllogin -portal-a "$portal_a" -portal-b "$portal_b" "$@" >"$out/$label.log" 2>&1 || { cat "$out/$label.log"; drill_fail "$label"; }
	grep -q '^PASS:' "$out/$label.log" || drill_fail "$label did not finish"
	drill_log "$label passed"
}
await_ready() {
	local name="$1" pid="$2" i
	for i in $(seq 1 180); do
		[ -f "$out/$name.ready" ] && return
		kill -0 "$pid" 2>/dev/null || { cat "$out/$name.log"; drill_fail "$name exited before barrier"; }
		sleep 1
	done
	drill_fail "$name ready timeout"
}
hold() {
	local label="$1"; shift
	bin/drilllogin -portal-a "$portal_a" -portal-b "$portal_b" -mode hold -ready "$out/$label.ready" -release "$out/$label.release" "$@" >"$out/$label.log" 2>&1 &
	probe_pid=$!
	probe_pids+=("$probe_pid")
	await_ready "$label" "$probe_pid"
}
crash_game() {
	local name="$1" pid
	pid="$(cat ".dev/$name.pid")"
	cp ".dev/$name.log" "$out/$name-before-kill.log"
	kill -9 "$pid"; wait "$pid" 2>/dev/null || true
	rm ".dev/$name.pid"
}

probe embedded-seed -endpoint "127.0.0.1:$(drill_port 7000)" -identity Embedded
# 两个 game 同时在线，真实 Directory 在不同 sid 上拒绝同名；失败后仍可正常建角、仅发一次金币。
probe cross-sid-naming -endpoint "127.0.0.1:$(drill_port 7001)" -sid 1001 -identity Other -mode collision -occupied EmbeddedR -create-name Embedded
hold embedded-restart -endpoint "127.0.0.1:$(drill_port 7000)" -identity Embedded
embedded_probe=$probe_pid
crash_game game
drill_start game bin/app game 1000 configs/service/config.game.yaml
touch "$out/embedded-restart.release"
wait "$embedded_probe" || { cat "$out/embedded-restart.log"; drill_fail 'embedded reconnect'; }
probe_pids=()
grep -q '^PASS:' "$out/embedded-restart.log" || drill_fail 'embedded reconnect missing PASS'

# 独立 Gate 的两个进程都有独立 sid、TCP / ops / discovery 地址；game 继续使用正式 generated Ingress。
drill_stop game
drill_ingress_config configs/service/config.game.yaml configs/service/config.game-ingress.yaml
awk '/^[a-z_]+:/{keep=($0 ~ /^(log|redis|nats|etcd|singleton|ops):/)} keep' configs/service/config.game.yaml >configs/service/config.gate.yaml
drill_edit -e "s/:$(drill_port 9100)\$/:$(drill_port 9300)/" -e "s/:$(drill_port 9000)\$/:$(drill_port 7100)/" configs/service/config.gate.yaml
sed -e "s/:$(drill_port 9300)\$/:$(drill_port 9301)/" -e "s/:$(drill_port 7100)\$/:$(drill_port 7101)/" configs/service/config.gate.yaml >configs/service/config.gate2.yaml
drill_start game-ingress bin/drillingress game 1000 configs/service/config.game-ingress.yaml
drill_start gate bin/drillgate gate 9500 configs/service/config.gate.yaml DRILL_GATE_ADDR="127.0.0.1:$(drill_port 7100)" DRILL_GATE_ADVERTISE="127.0.0.1:$(drill_port 7100)"
drill_start gate2 bin/drillgate gate 9501 configs/service/config.gate2.yaml DRILL_GATE_ADDR="127.0.0.1:$(drill_port 7101)" DRILL_GATE_ADVERTISE="127.0.0.1:$(drill_port 7101)"
probe gate-a-seed -endpoint "127.0.0.1:$(drill_port 7100)" -identity GateA
probe gate-b-seed -endpoint "127.0.0.1:$(drill_port 7101)" -identity GateB
hold gate-a-restart -endpoint "127.0.0.1:$(drill_port 7100)" -identity GateA
gate_a_probe=$probe_pid
hold gate-b-restart -endpoint "127.0.0.1:$(drill_port 7101)" -identity GateB
gate_b_probe=$probe_pid
crash_game game-ingress
drill_start game-ingress bin/drillingress game 1000 configs/service/config.game-ingress.yaml
touch "$out/gate-a-restart.release" "$out/gate-b-restart.release"
wait "$gate_a_probe" || { cat "$out/gate-a-restart.log"; drill_fail 'Gate A reconnect'; }
wait "$gate_b_probe" || { cat "$out/gate-b-restart.log"; drill_fail 'Gate B reconnect'; }
probe_pids=()
for label in gate-a-restart gate-b-restart; do grep -q '^PASS:' "$out/$label.log" || drill_fail "$label missing PASS"; done
# A 停掉以后，B 单独完成登录、选服、换票据；同一角色经另一个 Gate 进入并重放已落盘购买 / 改名回执。
drill_stop account
portal_a="$portal_b"
probe account-failover -endpoint "127.0.0.1:$(drill_port 7101)" -identity GateA -mode resume
crash_game gate
drill_start gate bin/drillgate gate 9500 configs/service/config.gate.yaml DRILL_GATE_ADDR="127.0.0.1:$(drill_port 7100)" DRILL_GATE_ADVERTISE="127.0.0.1:$(drill_port 7100)"
probe gate-process-restart -endpoint "127.0.0.1:$(drill_port 7100)" -identity GateA -mode resume
printf 'PASS: 9 real module phases; two account replicas, two game sids, two Gates, embedded/Gate crash reconnect, naming, servertime, purchase\n' | tee "$out/result.txt"
