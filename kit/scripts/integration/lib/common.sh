#!/usr/bin/env bash

# 测试根目录与端口（2026-09-29 起可配置，默认值与之前完全相同）。
#
# 根目录固定为 "$ROOST_IT_HOME/roost-dataengine-it"，ROOST_IT_HOME 缺省是 /tmp；
# 所有端口统一加 ROOST_IT_PORT_OFFSET（缺省 0）。之前根目录写死在 /tmp，而 macOS
# 会清理 /tmp 下长期没被访问的文件：旧环境的 mongo-3 在 2026-09-27 丢了一个
# WiredTiger 数据文件、pid 文件被删（docs/review/REVIEW-2026-09-29-b30.md「环境
# 异常」）。长期使用的环境应放到 /tmp 之外，并用偏移与仍在跑的旧环境错开端口。
#
# 只开放“父目录”而不开放任意根目录，是因为 fault / down / reset 会按根目录下的
# pid 文件杀进程、reset 会 rm -rf 根目录：根目录必须是绝对路径、basename 必须是
# roost-dataengine-it、不能是 / 或 $HOME 本身、不能含空白。ROOST_DATAENGINE_IT_ROOT
# （env.sh 导出的实际根）仍只能等于按这条规则算出的 canonical 根，否则一律拒绝。
#
# 同一平台差异照旧处理：/tmp 在 macOS 是 /private/tmp 的符号链接、在 Linux 是真实
# 目录（/private 在 Linux 上建不出来），`cd "$home" && pwd -P` 在两边都给出物理路径，
# 所以 ROOST_IT_HOME 必须是已存在的目录。
roost_it_config_error=""
roost_it_home_input="${ROOST_IT_HOME:-/tmp}"
roost_it_offset_input="${ROOST_IT_PORT_OFFSET:-0}"
roost_it_canonical_root_value=""
if [[ "$roost_it_home_input" != /* ]]; then
	roost_it_config_error="ROOST_IT_HOME must be an absolute path: $roost_it_home_input"
elif [[ "$roost_it_home_input" =~ [[:space:]] ]]; then
	roost_it_config_error="ROOST_IT_HOME must not contain whitespace: $roost_it_home_input"
elif [[ ! -d "$roost_it_home_input" ]]; then
	roost_it_config_error="ROOST_IT_HOME is not an existing directory (create it first): $roost_it_home_input"
else
	roost_it_canonical_root_value="$(cd "$roost_it_home_input" && pwd -P)"
	roost_it_canonical_root_value="${roost_it_canonical_root_value%/}/roost-dataengine-it"
	roost_it_home_physical=""
	if [[ -n "${HOME:-}" && -d "$HOME" ]]; then
		roost_it_home_physical="$(cd "$HOME" && pwd -P)"
	fi
	if [[ "$roost_it_canonical_root_value" != /* || "$(basename "$roost_it_canonical_root_value")" != "roost-dataengine-it" ]]; then
		roost_it_config_error="refuse unsafe root: $roost_it_canonical_root_value"
	elif [[ "$roost_it_canonical_root_value" == "/" || ( -n "$roost_it_home_physical" && "$roost_it_canonical_root_value" == "$roost_it_home_physical" ) ]]; then
		roost_it_config_error="refuse unsafe root: $roost_it_canonical_root_value"
	elif [[ "$roost_it_canonical_root_value" =~ [[:space:]] ]]; then
		roost_it_config_error="refuse unsafe root with whitespace: $roost_it_canonical_root_value"
	fi
fi
# 偏移只收十进制非负整数：bash 算术会把 0100 当八进制；最大端口 27119+偏移不得越过 65535。
if [[ -z "$roost_it_config_error" ]]; then
	if [[ ! "$roost_it_offset_input" =~ ^(0|[1-9][0-9]{0,4})$ ]] || (( roost_it_offset_input > 65535 - 27119 )); then
		roost_it_config_error="ROOST_IT_PORT_OFFSET must be a decimal integer in [0, $((65535 - 27119))]: $roost_it_offset_input"
	fi
fi
if [[ -n "$roost_it_config_error" ]]; then
	# 配置非法时仍给出一个不会命中任何真实目录的值，require_safe_root 负责拒绝。
	roost_it_canonical_root_value="/nonexistent/invalid-roost-it-config/roost-dataengine-it"
	roost_it_offset_input=0
fi
readonly ROOST_IT_CANONICAL_ROOT="$roost_it_canonical_root_value"
readonly ROOST_IT_HOME_RESOLVED="$(dirname "$ROOST_IT_CANONICAL_ROOT")"
readonly ROOST_IT_PORT_OFFSET_VALUE="$roost_it_offset_input"
# Go build cache for the suite, beside the test root: same platform rule.
readonly ROOST_IT_GO_CACHE_DEFAULT="$ROOST_IT_HOME_RESOLVED/roost-go-cache"
readonly ROOST_IT_ROOT="${ROOST_DATAENGINE_IT_ROOT:-$ROOST_IT_CANONICAL_ROOT}"
# 根目录与偏移绑定：同一根目录下的数据、pid 与 nats.conf 只对应一组端口。换偏移
# 复用旧根会让新端口的进程去抢同一数据目录，所以首次 up 记下偏移，之后不一致即拒绝；
# 已存在但没有记录的旧根按偏移 0 处理（之前唯一的取值）。
readonly ROOST_IT_PORT_OFFSET_FILE="$ROOST_IT_ROOT/port-offset"

# Remote 验收锁（REMOTE-ACCEPTANCE §共用集群串行）：scripts/test-remote-matrix.sh、
# scripts/perf/remote.sh 与本脚本的 test 用 mkdir 取得它，取得后导出
# ROOST_REMOTE_ACCEPTANCE_LOCK_HELD=<锁路径>，自己调起的 heal / remote-fault.sh 据此放行。
# RR-20261005-NC-203：之前只有持锁者自己看这把锁，别的会话在矩阵 / 长稳运行期间照常
# fault / down / up / heal / reset（reset 连锁目录带整个根一起删），验收结果失真。
readonly ROOST_IT_ACCEPTANCE_LOCK="$ROOST_IT_ROOT/remote-acceptance.lock"

# require_acceptance_lock_free_or_held：会改动环境的入口先调它；锁被别人持有时以 2 拒绝。
# 只读的 status 不调。锁是“目录存在即持有”，进程被强杀后残留的锁要先查清再手工删除。
require_acceptance_lock_free_or_held() {
	[[ -d "$ROOST_IT_ACCEPTANCE_LOCK" ]] || return 0
	[[ "${ROOST_REMOTE_ACCEPTANCE_LOCK_HELD:-}" == "$ROOST_IT_ACCEPTANCE_LOCK" ]] && return 0
	roost_it_error "refuse: $ROOST_IT_ACCEPTANCE_LOCK is held by a running Remote acceptance / fault run; wait for it (do not delete the lock until you know who holds it)"
	return 2
}

# acquire_acceptance_lock 让当前进程在退出前持有验收锁（已由上层持有时直接沿用）。
acquire_acceptance_lock() {
	if [[ "${ROOST_REMOTE_ACCEPTANCE_LOCK_HELD:-}" == "$ROOST_IT_ACCEPTANCE_LOCK" && -d "$ROOST_IT_ACCEPTANCE_LOCK" ]]; then
		return 0
	fi
	if ! mkdir "$ROOST_IT_ACCEPTANCE_LOCK" 2>/dev/null; then
		roost_it_error "refuse: $ROOST_IT_ACCEPTANCE_LOCK is held by a running Remote acceptance / fault run"
		return 2
	fi
	trap 'rmdir "$ROOST_IT_ACCEPTANCE_LOCK" 2>/dev/null || true' EXIT
	export ROOST_REMOTE_ACCEPTANCE_LOCK_HELD="$ROOST_IT_ACCEPTANCE_LOCK"
}

# roost_it_port 把基准端口平移 ROOST_IT_PORT_OFFSET；所有端口都必须经过它。
roost_it_port() {
	printf '%d\n' "$(($1 + ROOST_IT_PORT_OFFSET_VALUE))"
}

roost_it_log() {
	printf '[roost-it] %s\n' "$*"
}

roost_it_error() {
	printf '[roost-it] %s\n' "$*" >&2
}

require_safe_root() {
	if [[ -n "$roost_it_config_error" ]]; then
		roost_it_error "$roost_it_config_error"
		return 2
	fi
	if [[ "$ROOST_IT_ROOT" != "$ROOST_IT_CANONICAL_ROOT" ]]; then
		roost_it_error "refuse unsafe root: $ROOST_IT_ROOT (expected $ROOST_IT_CANONICAL_ROOT from ROOST_IT_HOME)"
		return 2
	fi
	# 根目录还不存在时是全新环境，任何合法偏移都可以；已存在的根按记录（无记录即 0）核对。
	[[ -d "$ROOST_IT_ROOT" ]] || return 0
	local recorded=0
	if [[ -f "$ROOST_IT_PORT_OFFSET_FILE" ]]; then
		recorded="$(tr -d '[:space:]' < "$ROOST_IT_PORT_OFFSET_FILE")"
	fi
	if [[ "$recorded" != "$ROOST_IT_PORT_OFFSET_VALUE" ]]; then
		roost_it_error "refuse port offset $ROOST_IT_PORT_OFFSET_VALUE: $ROOST_IT_ROOT was created with ROOST_IT_PORT_OFFSET=$recorded"
		return 2
	fi
}

# record_port_offset 在 up 建好根目录后写下偏移（已有记录时 require_safe_root 已核对过）。
record_port_offset() {
	[[ -f "$ROOST_IT_PORT_OFFSET_FILE" ]] && return 0
	printf '%s\n' "$ROOST_IT_PORT_OFFSET_VALUE" > "$ROOST_IT_PORT_OFFSET_FILE"
}

require_commands() {
	local missing=0 command_name
	for command_name in "$@"; do
		if ! command -v "$command_name" >/dev/null 2>&1; then
			roost_it_error "required command is missing: $command_name"
			missing=1
		fi
	done
	return "$missing"
}

wait_until() {
	local timeout="$1" description="$2"
	shift 2
	local deadline=$((SECONDS + timeout))
	until "$@"; do
		if (( SECONDS >= deadline )); then
			roost_it_error "timeout waiting for $description"
			return 1
		fi
		sleep 0.2
	done
}

pid_is_running() {
	local pid="$1"
	[[ "$pid" =~ ^[0-9]+$ ]] && kill -0 "$pid" >/dev/null 2>&1
}

pid_command() {
	ps -p "$1" -o command= 2>/dev/null || true
}

read_owned_pid() {
	local pid_file="$1"
	[[ -f "$pid_file" ]] || return 1
	local pid command_line
	pid="$(tr -d '[:space:]' < "$pid_file")"
	pid_is_running "$pid" || return 1
	command_line="$(pid_command "$pid")"
	# 按 " <根>/" 匹配而不是子串：根目录可配置后，/a/roost-dataengine-it 是
	# /x/a/roost-dataengine-it 的子串，单纯子串会把另一套环境的进程认成自己的。
	# 本脚本起的 mongod / nats-server / redis-server 命令行都含 " <根>/..." 参数。
	[[ "$command_line" == *" $ROOST_IT_ROOT/"* ]] || {
		roost_it_error "refuse foreign pid $pid from $pid_file: $command_line"
		return 2
	}
	printf '%s\n' "$pid"
}

stop_owned_pid() {
	local pid_file="$1" label="$2"
	local pid status=0
	pid="$(read_owned_pid "$pid_file")" || status=$?
	if [[ "$status" -eq 1 ]]; then
		rm -f -- "$pid_file"
		return 0
	fi
	if [[ "$status" -ne 0 ]]; then
		return "$status"
	fi
	roost_it_log "stopping $label (pid $pid)"
	kill -TERM "$pid"
	local deadline=$((SECONDS + 25))
	while pid_is_running "$pid"; do
		if (( SECONDS >= deadline )); then
			roost_it_error "$label did not stop after 25s"
			return 1
		fi
		sleep 0.2
	done
	rm -f -- "$pid_file"
}

port_is_listening() {
	nc -z -w 1 127.0.0.1 "$1" >/dev/null 2>&1
}

require_port_available_or_owned() {
	local port="$1" pid_file="$2" label="$3"
	if ! port_is_listening "$port"; then
		return 0
	fi
	if read_owned_pid "$pid_file" >/dev/null 2>&1; then
		return 0
	fi
	roost_it_error "$label port $port is occupied by a process outside $ROOST_IT_ROOT"
	return 1
}

write_environment_file() {
	mkdir -p "$ROOST_IT_ROOT"
	local output="$ROOST_IT_ROOT/env.sh" temporary="$ROOST_IT_ROOT/env.sh.tmp"
	{
		printf 'export ROOST_DATAENGINE_IT=1\n'
		printf 'export ROOST_DATAENGINE_IT_ROOT=%q\n' "$ROOST_IT_ROOT"
		# 同时导出父目录与偏移：source 过 env.sh 的 shell 再调 heal / fault /
		# scripts/remote-fault.sh 时指向的就是这套环境，而不是默认的 /tmp 那套。
		printf 'export ROOST_IT_HOME=%q\n' "$ROOST_IT_HOME_RESOLVED"
		printf 'export ROOST_IT_PORT_OFFSET=%q\n' "$ROOST_IT_PORT_OFFSET_VALUE"
		printf 'export ROOST_DATAENGINE_IT_MONGO_URI=%q\n' "$ROOST_IT_MONGO_URI"
		printf 'export ROOST_DATAENGINE_IT_NATS_URL=%q\n' "$(nats_client_url)"
		printf 'export ROOST_DATAENGINE_IT_REDIS_ADDR=%q\n' "$(redis_addr)"
		if toxiproxy_running; then
			printf 'export ROOST_DATAENGINE_IT_TOXIPROXY_URL=%q\n' "$(toxiproxy_api)"
			printf 'export ROOST_DATAENGINE_IT_NATS_PROXIED_URL=%q\n' "$(toxiproxy_nats_url)"
			printf 'export ROOST_DATAENGINE_IT_REDIS_PROXIED_ADDR=%q\n' "$(toxiproxy_redis_addr)"
		fi
	} > "$temporary"
	mv -f -- "$temporary" "$output"
}
