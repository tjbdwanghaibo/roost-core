#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
script="$repo_root/scripts/integration/dataengine-env.sh"
lib="$repo_root/scripts/integration/lib"

if [[ ! -x "$script" ]]; then
	echo "dataengine environment script is missing or not executable: $script" >&2
	exit 1
fi

fail() {
	echo "$*" >&2
	exit 1
}

# status 只读：拒绝类用例都走 status 而不是 reset，校验本身出错时也不会删任何东西。
status_code=0
"$script" status >/dev/null 2>&1 || status_code=$?
if [[ "$status_code" -ne 0 && "$status_code" -ne 1 ]]; then
	fail "status returned unexpected code $status_code"
fi

unsafe_output="$(ROOST_DATAENGINE_IT_ROOT=/tmp/not-roost "$script" reset 2>&1 || true)"
if [[ "$unsafe_output" != *"refuse unsafe root"* ]]; then
	fail "reset did not reject unsafe root: $unsafe_output"
fi

# expect_refused NAME PATTERN ENV... ：带给定环境变量跑 status，必须以 2 拒绝且报出 PATTERN。
expect_refused() {
	local name="$1" pattern="$2" output code=0
	shift 2
	output="$(env -u ROOST_IT_HOME -u ROOST_IT_PORT_OFFSET -u ROOST_DATAENGINE_IT_ROOT "$@" "$script" status 2>&1)" || code=$?
	[[ "$code" -eq 2 && "$output" == *"$pattern"* ]] || fail "$name: expected refusal ($pattern), got code $code: $output"
}

scratch="$(mktemp -d "${TMPDIR:-/tmp}/roost-it-shelltest.XXXXXX")"
trap 'rm -rf -- "$scratch"' EXIT
scratch="$(cd "$scratch" && pwd -P)"
mkdir -p "$scratch/with space"

# ROOST_IT_HOME（2026-09-29）：绝对、已存在、无空白。
expect_refused "relative home" "must be an absolute path" ROOST_IT_HOME=relative/dir
expect_refused "home with space" "must not contain whitespace" "ROOST_IT_HOME=$scratch/with space"
expect_refused "missing home" "not an existing directory" "ROOST_IT_HOME=$scratch/missing"
# ROOST_DATAENGINE_IT_ROOT 只能等于按 ROOST_IT_HOME 算出的 canonical 根。
expect_refused "root outside home" "refuse unsafe root" "ROOST_IT_HOME=$scratch" "ROOST_DATAENGINE_IT_ROOT=/tmp/roost-dataengine-it-other"
expect_refused "home root under default" "refuse unsafe root" "ROOST_IT_HOME=$scratch" "ROOST_DATAENGINE_IT_ROOT=$(cd /tmp && pwd -P)/roost-dataengine-it"

# ROOST_IT_PORT_OFFSET：十进制非负整数，且最大端口不越界。
expect_refused "non-numeric offset" "ROOST_IT_PORT_OFFSET must be" "ROOST_IT_HOME=$scratch" ROOST_IT_PORT_OFFSET=abc
expect_refused "octal-looking offset" "ROOST_IT_PORT_OFFSET must be" "ROOST_IT_HOME=$scratch" ROOST_IT_PORT_OFFSET=0100
expect_refused "negative offset" "ROOST_IT_PORT_OFFSET must be" "ROOST_IT_HOME=$scratch" ROOST_IT_PORT_OFFSET=-1
expect_refused "offset past 65535" "ROOST_IT_PORT_OFFSET must be" "ROOST_IT_HOME=$scratch" ROOST_IT_PORT_OFFSET=38417

# 根目录与偏移绑定：记录是 5 的根拒绝以 0 或 7 操作。
mkdir -p "$scratch/roost-dataengine-it"
printf '5\n' > "$scratch/roost-dataengine-it/port-offset"
expect_refused "offset mismatch" "was created with ROOST_IT_PORT_OFFSET=5" "ROOST_IT_HOME=$scratch" ROOST_IT_PORT_OFFSET=7
expect_refused "default offset on recorded root" "was created with ROOST_IT_PORT_OFFSET=5" "ROOST_IT_HOME=$scratch"
# 已存在却没有记录的根是改动前建的，只认偏移 0。
rm -f -- "$scratch/roost-dataengine-it/port-offset"
expect_refused "legacy root" "was created with ROOST_IT_PORT_OFFSET=0" "ROOST_IT_HOME=$scratch" ROOST_IT_PORT_OFFSET=1000
rm -rf -- "$scratch/roost-dataengine-it"

# 端口表与 env.sh：只 source 库、只写临时根，不启动任何进程。
# port_table HOME OFFSET 打印 canonical 根、全部端口与 env.sh 内容。
port_table() {
	ROOST_IT_HOME="$1" ROOST_IT_PORT_OFFSET="$2" bash -c '
set -euo pipefail
unset ROOST_DATAENGINE_IT_ROOT
source "$0/common.sh"; source "$0/mongo.sh"; source "$0/nats.sh"; source "$0/redis.sh"; source "$0/toxiproxy.sh"
require_safe_root
printf "root=%s\n" "$ROOST_IT_CANONICAL_ROOT"
printf "mongo=%s,%s,%s\n" "$(mongo_node_port 1)" "$(mongo_node_port 2)" "$(mongo_node_port 3)"
printf "nats_client=%s,%s,%s\n" "$(nats_client_port 1)" "$(nats_client_port 2)" "$(nats_client_port 3)"
printf "nats_route=%s,%s,%s\n" "$(nats_route_port 1)" "$(nats_route_port 2)" "$(nats_route_port 3)"
printf "nats_monitor=%s,%s,%s\n" "$(nats_monitor_port 1)" "$(nats_monitor_port 2)" "$(nats_monitor_port 3)"
printf "redis=%s\n" "$(redis_port)"
printf "toxiproxy=%s,%s,%s,%s,%s\n" "$(toxiproxy_api_port)" "$(toxiproxy_proxy_port 1)" "$(toxiproxy_proxy_port 2)" "$(toxiproxy_proxy_port 3)" "$(toxiproxy_redis_port)"
if [[ "$ROOST_IT_CANONICAL_ROOT" != "$(cd /tmp && pwd -P)/roost-dataengine-it" ]]; then
	mkdir -p "$ROOST_IT_ROOT"
	record_port_offset
	write_environment_file
	cat "$ROOST_IT_ROOT/env.sh"
fi
' "$lib"
}

# 默认值：与改动前完全一致（根在 /tmp，端口不变）。不写 env.sh，避免碰默认根。
# 空值与未设置等价（${VAR:-default}）。
default_table="$(port_table "" "")"
expected_default="root=$(cd /tmp && pwd -P)/roost-dataengine-it
mongo=27117,27118,27119
nats_client=14222,14223,14224
nats_route=16222,16223,16224
nats_monitor=18222,18223,18224
redis=16379
toxiproxy=18474,24222,24223,24224,26379"
[[ "$default_table" == "$expected_default" ]] || fail "default ports changed:
$default_table"

shifted_table="$(port_table "$scratch" 1000)"
expected_shifted="root=$scratch/roost-dataengine-it
mongo=28117,28118,28119
nats_client=15222,15223,15224
nats_route=17222,17223,17224
nats_monitor=19222,19223,19224
redis=17379
toxiproxy=19474,25222,25223,25224,27379"
[[ "$shifted_table" == "$expected_shifted"* ]] || fail "offset 1000 ports wrong:
$shifted_table"
for line in \
	"export ROOST_DATAENGINE_IT_ROOT=$scratch/roost-dataengine-it" \
	"export ROOST_IT_HOME=$scratch" \
	"export ROOST_IT_PORT_OFFSET=1000" \
	"export ROOST_DATAENGINE_IT_MONGO_URI=mongodb://127.0.0.1:28117\\,127.0.0.1:28118\\,127.0.0.1:28119/\\?replicaSet=roost-it" \
	"export ROOST_DATAENGINE_IT_NATS_URL=nats://127.0.0.1:15222\\,nats://127.0.0.1:15223\\,nats://127.0.0.1:15224" \
	"export ROOST_DATAENGINE_IT_REDIS_ADDR=127.0.0.1:17379"; do
	grep -qxF -- "$line" <<<"$shifted_table" || fail "env.sh is missing: $line
$shifted_table"
done

# 生成的 env.sh 被 source 后，同一脚本认得这套根（heal / fault 走到同一环境）。
sourced_output="$(env -u ROOST_IT_HOME -u ROOST_IT_PORT_OFFSET -u ROOST_DATAENGINE_IT_ROOT bash -c '
set -euo pipefail
source "$1/roost-dataengine-it/env.sh"
source "$2/common.sh"
require_safe_root && printf "%s %s\n" "$ROOST_IT_ROOT" "$(roost_it_port 27117)"
' _ "$scratch" "$lib")"
[[ "$sourced_output" == "$scratch/roost-dataengine-it 28117" ]] || fail "sourced env.sh does not select its own root: $sourced_output"

# 验收锁（维护者决定 A5，接 RR-20261005-NC-203）：会改动整套环境的全局入口在运行期间持有
# <根>/remote-acceptance.lock，别人持有时以 2 拒绝，持锁者的子进程沿用。用临时根（偏移 31000，没有任何进程）
# 加 PATH 垫片观察：垫片命令被调用时记下锁目录在不在，然后失败退出，所以不会碰到任何真实服务。
# 修前：fault / heal / remote-fault 只检查锁空闲、自己不持有，垫片记下的是 free。
lock_home="$scratch/lockhome"
lock_root="$lock_home/roost-dataengine-it"
lock_dir="$lock_root/remote-acceptance.lock"
shims="$scratch/shims"
observed="$scratch/observed"
mkdir -p "$lock_root" "$shims"
printf '31000\n' > "$lock_root/port-offset"
for shim in mongod mongosh nats-server redis-server redis-cli curl jq nc go; do
	cat > "$shims/$shim" <<SHIM
#!/usr/bin/env bash
if [[ -d "$lock_dir" ]]; then state=held; else state=free; fi
printf '%s lock=%s\\n' "$shim" "\$state" >> "$observed"
exit 1
SHIM
	chmod +x "$shims/$shim"
done
# run_locked NAME COMMAND...：在临时根上跑，返回退出码；垫片记录写进 $observed。
run_locked() {
	local code=0
	: > "$observed"
	env -u ROOST_REMOTE_ACCEPTANCE_LOCK_HELD PATH="$shims:$PATH" ROOST_IT_HOME="$lock_home" ROOST_IT_PORT_OFFSET=31000 \
		ROOST_DATAENGINE_IT_ROOT="$lock_root" ROOST_DATAENGINE_IT=1 \
		ROOST_DATAENGINE_IT_MONGO_URI=mongodb://127.0.0.1:1 ROOST_DATAENGINE_IT_REDIS_ADDR=127.0.0.1:1 ROOST_DATAENGINE_IT_NATS_URL=nats://127.0.0.1:1 \
		"$@" >/dev/null 2>&1 || code=$?
	return "$code"
}
expect_held_while_running() {
	local name="$1"
	shift
	run_locked "$@" || true
	[[ -s "$observed" ]] || fail "$name: no shim was reached, the check observed nothing"
	! grep -q 'lock=free' "$observed" || fail "$name: ran without holding the acceptance lock:
$(cat "$observed")"
	[[ ! -d "$lock_dir" ]] || fail "$name: left $lock_dir behind after exiting"
}
expect_held_while_running "fault mongo-primary" "$script" fault mongo-primary
expect_held_while_running "heal" "$script" heal
expect_held_while_running "remote-fault.sh mongo-primary" bash "$repo_root/../scripts/remote-fault.sh" mongo-primary
expect_held_while_running "test-remote-generated.sh with ROOST_REMOTE_FAULT" env ROOST_REMOTE_FAULT=nats-node bash "$repo_root/../scripts/test-remote-generated.sh"
# 别人持锁：拒绝（2），不碰服务，锁原样保留。
mkdir "$lock_dir"
for command in "fault mongo-primary" "heal" "down"; do
	code=0
	# shellcheck disable=SC2086
	run_locked "$script" $command || code=$?
	[[ "$code" -eq 2 && ! -s "$observed" && -d "$lock_dir" ]] || fail "$command while another run holds the lock: code $code, observed $(cat "$observed")"
done
# 持锁者的子进程（ROOST_REMOTE_ACCEPTANCE_LOCK_HELD 指向这把锁）沿用它，退出时不释放。
: > "$observed"
env PATH="$shims:$PATH" ROOST_IT_HOME="$lock_home" ROOST_IT_PORT_OFFSET=31000 ROOST_DATAENGINE_IT_ROOT="$lock_root" \
	ROOST_REMOTE_ACCEPTANCE_LOCK_HELD="$lock_dir" "$script" fault mongo-primary >/dev/null 2>&1 || true
grep -q 'mongosh lock=held' "$observed" || fail "the holder's child was refused: $(cat "$observed")"
[[ -d "$lock_dir" ]] || fail "the holder's child released the holder's lock"
rmdir "$lock_dir"

echo "dataengine environment shell tests passed"
