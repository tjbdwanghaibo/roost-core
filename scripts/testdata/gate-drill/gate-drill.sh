#!/usr/bin/env bash
# roost add gate 生成工程的单机真实进程演练：-template game + login enter portal + roost add gate，
# account、game（Game Ingress）、gate 三个进程，真实 NATS / Redis / Mongo（私有 mirror-local）与私有 etcd，
# 经登录入口拿进服票据、经 Gate 握手并新建 / 进入一次。结束时停掉全部进程、删本工程的库 / 流 / 键。
# 不进 pretag（要真实依赖）；改 roost add gate、生成的 Gate / Ingress Mod 或 wiring/gate 时手动跑。
#
#   ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=19000 scripts/mirror-local.sh up
#   source <私有目录>/roost-dataengine-it/env.sh
#   DEMO_LOGIN_OUT=/abs/dir bash scripts/testdata/gate-drill/gate-drill.sh
#   ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=19000 scripts/mirror-local.sh clean
#
# DRILL_PORT_SHIFT（缺省 31500）：开发端口平移量；DRILL_ETCD_PORT（缺省 41479）。
# DRILL_GATE_NAME（缺省 gate）：Gate 服务名；写别的名字（例如 edge）就是 roost add gate edge 的演练——Gate 进程按
# 实际服务名持单实例锁、登记发现，握手必须照样通过（RV-20261116-02）。DRILL_PLAYER_SERVICE（缺省 game）：玩家服务名
# （project new -services <名字>），自定义玩家服务名同理。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?}" "${ROOST_DATAENGINE_IT_NATS_URL:?}" "${ROOST_DATAENGINE_IT_REDIS_ADDR:?}" "${DEMO_LOGIN_OUT:?}"
drill_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd "$drill_dir/../../.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-gate-drill.XXXXXX")"
project="$(printf 'gtd%06x' $(((RANDOM << 9) ^ RANDOM)))"
out="$DEMO_LOGIN_OUT"
port_shift="${DRILL_PORT_SHIFT:-31500}"
etcd_port="${DRILL_ETCD_PORT:-41479}"
etcd_pid=""
gate_name="${DRILL_GATE_NAME:-gate}"
player="${DRILL_PLAYER_SERVICE:-game}"
mkdir -p "$out"
export GOWORK=off GOFLAGS="-mod=mod"
# shellcheck source=/dev/null
source "$repo_dir/scripts/testdata/demo-drill/lib.sh"
trap drill_cleanup EXIT
{ go version; git -C "$repo_dir" rev-parse HEAD; printf 'project=%s port_shift=%s etcd_port=%s gate_name=%s player=%s\n' "$project" "$port_shift" "$etcd_port" "$gate_name" "$player"; } >"$out/env.txt"
drill_check_ports
drill_start_etcd
(cd "$repo_dir" && go build -o "$out/roost" ./codegen/cmd/roost)
"$out/roost" project new "$project" -module "example.com/$project" -out "$work_dir/$project" -template game -services "$player" --skip-deps >"$out/generate.log" 2>&1
cd "$work_dir/$project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_dir"
"$out/roost" module add login enter portal >>"$out/generate.log" 2>&1
if [[ "$gate_name" == gate ]]; then "$out/roost" add gate; else "$out/roost" add gate "$gate_name"; fi >"$out/add-gate.log" 2>&1
mkdir -p cmd/gatelogin
sed "s|DRILL_MODULE|example.com/$project|g" "$drill_dir/gatelogin.go.txt" >cmd/gatelogin/main.go
go mod tidy >"$out/tidy.log" 2>&1
mkdir -p bin
go build -o bin/app . && go build -o bin/accountctl ./cmd/accountctl && go build -o bin/gatelogin ./cmd/gatelogin
drill_configure
printf 'login:\n  dev_token: dev-gate-drill\n' >>configs/service/config.account.yaml
cp configs/service/config.*.yaml "$out/"
"$out/roost" project doctor >"$out/doctor.log" 2>&1 || { tail -40 "$out/doctor.log"; drill_fail "doctor on the configured project"; }
grep -E '^(OK|FAIL|WARN) +gate:' "$out/doctor.log"
prefix="$(awk '/^account:/{a=1;next} a&&/^  key_prefix:/{print $2;exit}' configs/service/config.account.yaml)"
bin/accountctl -redis "$ROOST_DATAENGINE_IT_REDIS_ADDR" -prefix "$prefix" upsert-server -sid 1000 >"$out/register.log" 2>&1
drill_start acct bin/app account 1000 configs/service/config.account.yaml
drill_start game1000 bin/app "$player" 1000 "configs/service/config.$player.yaml"
drill_start gate1000 bin/app "$gate_name" 1000 "configs/service/config.$gate_name.yaml"
gate_addr="127.0.0.1:$(drill_port 7000)"
listener="$(lsof -nP -iTCP:"$(drill_port 7000)" -sTCP:LISTEN -t | head -1)"
[[ "$listener" == "$(cat .dev/gate1000.pid)" ]] || drill_fail "client port $(drill_port 7000) is held by pid $listener, not the Gate $(cat .dev/gate1000.pid)"
drill_log "client port $gate_addr is the Gate's (pid $listener); game pid $(cat .dev/game1000.pid) does not listen on it"
bin/gatelogin -portal "http://127.0.0.1:$(drill_port 8081)" -identity "G$project" -dev-token dev-gate-drill -want-addr "$gate_addr" | tee "$out/gatelogin.log"
grep -q '^PASS:' "$out/gatelogin.log" || drill_fail "gatelogin did not pass"
for name in acct game1000 gate1000; do
	n=$(grep -c '"level":"ERROR"' ".dev/$name.log" || true)
	drill_log "$name: $n ERROR log lines"
done
grep -h 'game ingress\|access.game_ingress\|access.gate' .dev/*.log | head -5 >"$out/ingress-lines.log" || true
drill_log "PASS gate drill"
