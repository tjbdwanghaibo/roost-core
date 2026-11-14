#!/usr/bin/env bash
# game-demo 登录链路的真实进程演练（可重复；KNOWN-LIMITS“game-demo 接入登录链路的边界”的验证范围）。
#
# 生成当前检出的 game-demo，开发配置改指向私有依赖、端口整体平移（scripts/testdata/demo-drill/lib.sh），然后：
#   1. `make dev-run`：deploy/dev/run.sh 起全部 11 个服务（九个托管服务、alliance、game 1000），并用 cmd/accountctl 自动
#      登记 1000 服；
#   2. curl 走登录入口三步（/portal/login → /portal/servers → /portal/enter），断言推荐 1000 服、地址是本次平移后的
#      game 地址、新号 role 为空、票据是 enter:<…>；开发渠道（login.dev_token）登录成功；demo 渠道凭据不对答 560102；
#   3. 机器人 `-login portal`：缺省场景（新号：握手 → EnterStatus → EnterCreate → EnterGame → 整个场景，含改名三步）与
#      alliance_offline_join（同一个身份第二次登录时入口清单里已有角色：老号路径）；
#   4. 机器人缺省的总线登录：同两个场景（即 make dev-smoke）；
#   5. 独立 Gate（DEMO_LOGIN_GATE=0 时跳过）：停掉 game 1000，换成 Game Ingress 接法（cmd/drillingress，嵌入 TCP 关闭）并起
#      独立 Gate（cmd/drillgate），机器人经 Gate 跑 -login portal 与总线登录（DEMO_LOGIN_GATE_SCENARIO，缺省是整个缺省场景 demo）。
#   6. 全新 casual + naming 工程：双 account、双 game sid、双 Gate、真实命名与购买回执，game / Gate SIGKILL 后重连，
#      ServertimeNow 的真实收包次序。DEMO_LOGIN_MODULES=0 只跑 demo；DEMO_LOGIN_MODULES_ONLY=1 只跑这一阶段。
# 结束时停掉全部进程、删本工程的库 / 流 / 键；证据（curl 应答、机器人报告与输出、各进程日志）留在 $DEMO_LOGIN_OUT。
#
#   ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=<偏移> scripts/mirror-local.sh up
#   source <私有目录>/roost-dataengine-it/env.sh
#   DEMO_LOGIN_OUT=/abs/dir scripts/test-demo-login.sh
#
# DEMO_LOGIN_PORT_SHIFT（缺省 30000）：开发端口整体平移量；DEMO_LOGIN_ETCD_PORT（缺省 34479）；DEMO_LOGIN_ROBOTS（缺省 2）；
# DEMO_LOGIN_PROJECT：工程名（缺省每次随机 lgd<六位十六进制>）；DRILL_KEEP=1 结束时不清理。不进 pretag。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo replica-set URI required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?private Redis required}"
: "${DEMO_LOGIN_OUT:?evidence directory required (absolute)}"
for tool in etcd jq curl mongosh redis-cli nats; do
	command -v "$tool" >/dev/null || { echo "$tool not found on PATH" >&2; exit 2; }
done
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ "${DEMO_LOGIN_MODULES_ONLY:-0}" == 1 ]]; then
	exec bash "$repo_dir/scripts/testdata/demo-drill/login-modules.sh"
fi
if [[ "${DRILL_KEEP:-0}" == 1 && "${DEMO_LOGIN_MODULES:-1}" == 1 ]]; then
	echo "DRILL_KEEP=1 requires DEMO_LOGIN_MODULES_ONLY=1 or DEMO_LOGIN_MODULES=0 (the two stages reuse the same ports)" >&2
	exit 2
fi
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-demo-login.XXXXXX")"
project="${DEMO_LOGIN_PROJECT:-$(printf 'lgd%06x' $(((RANDOM << 9) ^ RANDOM)))}"
out="$DEMO_LOGIN_OUT"
port_shift="${DEMO_LOGIN_PORT_SHIFT:-30000}"
etcd_port="${DEMO_LOGIN_ETCD_PORT:-34479}"
robots="${DEMO_LOGIN_ROBOTS:-2}"
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

# 1. make dev-run：11 个服务，自动登记 1000 服。
drill_log "make dev-run"
make dev-run >"$out/dev-run.log" 2>&1 || { tail -30 "$out/dev-run.log"; drill_fail "make dev-run"; }
grep -q "registering game server 1000" "$out/dev-run.log" || drill_fail "dev-run did not register server 1000"
running=$(sh deploy/dev/run.sh status | grep -c ': running')
[ "$running" = 11 ] || { sh deploy/dev/run.sh status; drill_fail "$running of 11 services running"; }
drill_log "11 services running, server 1000 registered"

# 2. curl 三步。
portal="http://127.0.0.1:$(drill_port 8081)"
game_addr="127.0.0.1:$(drill_port 7000)"
post() { curl -s -X POST "$portal$1" -H 'Content-Type: application/json' -d "$2"; }
open_id="curl-$project"
post /portal/login "{\"channel\":\"demo\",\"open_id\":\"$open_id\",\"credential\":\"demo:$open_id\"}" >"$out/curl-1-login.json"
account_ticket=$(jq -r 'select(.ok == true) | .account_ticket' "$out/curl-1-login.json")
[ -n "$account_ticket" ] || { cat "$out/curl-1-login.json"; drill_fail "/portal/login"; }
post /portal/servers "{\"account_ticket\":\"$account_ticket\"}" >"$out/curl-2-servers.json"
jq -e --arg addr "$game_addr" '.ok == true and .recommended_server_id == 1000 and
	([.servers[] | select(.server_id == 1000 and .status == "open" and .addr == $addr and .role == null)] | length == 1)' \
	"$out/curl-2-servers.json" >/dev/null || { cat "$out/curl-2-servers.json"; drill_fail "/portal/servers"; }
post /portal/enter "{\"account_ticket\":\"$account_ticket\",\"server_id\":1000}" >"$out/curl-3-enter.json"
jq -e --arg addr "$game_addr" '.ok == true and (.ticket | startswith("enter:")) and .server_id == 1000 and .serving_server_id == 1000 and .addr == $addr' \
	"$out/curl-3-enter.json" >/dev/null || { cat "$out/curl-3-enter.json"; drill_fail "/portal/enter"; }
post /portal/login '{"channel":"dev","open_id":"dev-'"$project"'","credential":"dev-local"}' >"$out/curl-dev-login.json"
jq -e '.ok == true and (.account_ticket | length > 0)' "$out/curl-dev-login.json" >/dev/null || { cat "$out/curl-dev-login.json"; drill_fail "dev channel login"; }
post /portal/login "{\"channel\":\"demo\",\"open_id\":\"$open_id\",\"credential\":\"demo:someone-else\"}" >"$out/curl-bad-credential.json"
jq -e '.ok == false and .code == 560102' "$out/curl-bad-credential.json" >/dev/null || { cat "$out/curl-bad-credential.json"; drill_fail "a wrong credential was not refused with 560102"; }
drill_log "curl: login, server list (1000 recommended at $game_addr, no role), enter ticket; dev channel; wrong credential 560102"

# 3–4. 机器人：-login portal 与总线登录，缺省场景与 alliance_offline_join。
endpoint=(-endpoint "$game_addr" -server-id 1000)
drill_robots portal-demo "${endpoint[@]}" -login portal -portal "$portal" -count "$robots"
drill_robots portal-offline-join "${endpoint[@]}" -login portal -portal "$portal" -scenario alliance_offline_join -count "$robots"
drill_robots bus-demo "${endpoint[@]}" -count "$robots"
drill_robots bus-offline-join "${endpoint[@]}" -scenario alliance_offline_join -count "$robots"

# 5. 可选：独立 Gate。
if [[ "${DEMO_LOGIN_GATE:-1}" == 1 ]]; then
	drill_log "independent Gate: game 1000 behind Game Ingress"
	sh deploy/dev/run.sh stop game >/dev/null
	drill_ingress_config configs/service/config.game.yaml configs/service/config.game-ingress.yaml
	drill_gate_config
	drill_start game-ingress bin/drillingress game 1000 configs/service/config.game-ingress.yaml
	drill_start gate bin/drillgate gate 9500 configs/service/config.gate.yaml \
		DRILL_GATE_ADDR="127.0.0.1:$(drill_port 7100)" DRILL_GATE_ADVERTISE="127.0.0.1:$(drill_port 7100)"
	gate=(-endpoint "127.0.0.1:$(drill_port 7100)" -server-id 1000)
	gate_scenario="${DEMO_LOGIN_GATE_SCENARIO:-demo}"
	drill_robots gate-portal "${gate[@]}" -login portal -portal "$portal" -scenario "$gate_scenario" -count "$robots"
	drill_robots gate-bus "${gate[@]}" -scenario "$gate_scenario" -count "$robots"
fi

errors=0
for log in .dev/*.log; do
	n=$(grep -c '"level":"ERROR"' "$log" || true)
	[ "$n" = 0 ] || { drill_log "$(basename "$log"): $n ERROR lines"; errors=$((errors + n)); }
done
drill_log "PASS demo login drill ($errors ERROR log lines in total; logs in $out/logs)"
if [[ "${DEMO_LOGIN_MODULES:-1}" == 1 ]]; then
	# 顺序释放 demo 的端口，再以全新前缀验证正式模块组合；公共清理只操作本次生成工程。
	drill_cleanup
	trap - EXIT
	DEMO_LOGIN_OUT="$out/modules" bash "$repo_dir/scripts/testdata/demo-drill/login-modules.sh"
fi
