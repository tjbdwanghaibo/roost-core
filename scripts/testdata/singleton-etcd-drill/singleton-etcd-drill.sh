#!/usr/bin/env bash
# 单实例锁存 etcd 的单机真实进程演练（docs/maintenance/FEATURE-2026-11-16-singleton-etcd.md §8，沿用 2026-10-05 单实例锁演练的做法）：
# -template game 生成工程（game 带 DataEngine，singleton.enabled: true，锁经 kitetcd.SingletonStore 存在私有 etcd），
# 真实 NATS / Redis / Mongo（私有 mirror-local）与本脚本自起的私有 etcd。依次验证：
#
#   1. 同 sid 两个进程：A 持锁就绪；B 阻塞在启动等待（不就绪、日志在等持有者），A 照常就绪。
#   2. SIGSTOP A：A 的租约过期（ttl 内不再续）后 B 接管（拿到锁、就绪）；接管时刻不早于 ttl − renew_interval。
#   3. SIGCONT A：A 下一次续锁拿到“不是我的”，fail-stop（非零退出，日志有 "singleton lock lost; fail-stop"）；B 不受影响。
#   4. etcd 停 20s（kill，而不是 SIGSTOP：连接被拒绝）：持锁的 B 在窗口（ttl）内 fail-stop，不等 etcd 恢复；
#      etcd 用同一数据目录恢复后，同 sid 重新拉起的进程在启动等待内拿到锁、就绪。
#
# B 用另一个工作目录（WAL 缺省 data/wal/<服务>/<sid> 相对工作目录）：同一 WAL 目录时 B 拿到锁后会在 WAL flock 处退出
# （冻结的 A 仍持有 flock），那条路径由 2026-10-05 的演练覆盖；这里看的是锁本身的接管。
# 不进 pretag（要真实依赖）；改 kitetcd.SingletonStore、framework/app 的单实例锁或生成的 singleton 配置时手动跑。
#
#   ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=19000 scripts/mirror-local.sh up
#   source <私有目录>/roost-dataengine-it/env.sh
#   SINGLETON_DRILL_OUT=/abs/dir bash scripts/testdata/singleton-etcd-drill/singleton-etcd-drill.sh
#   ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=19000 scripts/mirror-local.sh clean
#
# DRILL_PORT_SHIFT（缺省 33500）：开发端口平移量；第二个进程的监听端口再加 1000。DRILL_ETCD_PORT（缺省 44479）。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?}" "${ROOST_DATAENGINE_IT_NATS_URL:?}" "${ROOST_DATAENGINE_IT_REDIS_ADDR:?}" "${SINGLETON_DRILL_OUT:?}"
drill_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd "$drill_dir/../../.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-singleton-drill.XXXXXX")"
project="$(printf 'sgd%06x' $(((RANDOM << 9) ^ RANDOM)))"
out="$SINGLETON_DRILL_OUT"
port_shift="${DRILL_PORT_SHIFT:-33500}"
etcd_port="${DRILL_ETCD_PORT:-44479}"
etcd_pid=""
sid=1000
mkdir -p "$out"
export GOWORK=off GOFLAGS="-mod=mod"
# shellcheck source=/dev/null
source "$repo_dir/scripts/testdata/demo-drill/lib.sh"

# 本演练起的 game 进程（pid 在 $pids_dir/<名字>）：收尾时先 SIGCONT（被冻结的进程不处理 SIGTERM），再停，最后交给 drill_cleanup。
pids_dir="$work_dir/pids"
mkdir -p "$pids_dir"
drill_singleton_cleanup() {
	set +e
	local f pid
	for f in "$pids_dir"/*; do
		[ -f "$f" ] || continue
		case "$f" in *.exited) continue ;; esac
		pid=$(cat "$f")
		kill -CONT "$pid" 2>/dev/null
		kill "$pid" 2>/dev/null
	done
	sleep 2
	for f in "$pids_dir"/*; do
		[ -f "$f" ] || continue
		case "$f" in *.exited) continue ;; esac
		kill -9 "$(cat "$f")" 2>/dev/null
	done
	mkdir -p "$out/logs"
	cp "$work_dir"/*.log "$out/logs/" 2>/dev/null
	cp "$work_dir/etcd/etcd.log" "$out/logs/" 2>/dev/null
	drill_cleanup
}
trap drill_singleton_cleanup EXIT

# yaml_value <文件> <段> <键>：顶层段下一层键的值（去掉引号）。
yaml_value() {
	awk -v section="$2:" -v key="  $3:" '$0==section{s=1;next} s&&/^[^ #]/{s=0} s&&index($0,key)==1{sub(/^[^:]*: */,"");gsub(/"/,"");print;exit}' "$1"
}

# start_game <名字> <工作目录> <配置>：在工作目录里后台起 game（不等就绪），pid 记下。在当前 shell 里起，wait 才拿得到退出码。
start_game() {
	local name="$1" dir="$2" config="$3"
	pushd "$dir" >/dev/null
	nohup "$work_dir/$project/bin/app" game --sid "$sid" --config "$config" >"$work_dir/$name.log" 2>&1 &
	echo $! >"$pids_dir/$name"
	popd >/dev/null
	drill_log "$name started (pid $(cat "$pids_dir/$name"), dir $dir)"
}

pid_of() { cat "$pids_dir/$1"; }

ready() { curl -fsS -o /dev/null --max-time 2 "http://$1/readyz" 2>/dev/null; }

# wait_ready <名字> <ops> <秒>：等就绪，输出用了多少秒。进程先退出就失败。
wait_ready() {
	local name="$1" ops="$2" limit="$3" i=0
	while [ "$i" -lt "$limit" ]; do
		ready "$ops" && { echo "$i"; return 0; }
		kill -0 "$(pid_of "$name")" 2>/dev/null || { tail -20 "$work_dir/$name.log" >&2; drill_fail "$name exited before it was ready"; }
		i=$((i + 1))
		sleep 1
	done
	tail -20 "$work_dir/$name.log" >&2
	drill_fail "$name not ready in ${limit}s"
}

# wait_exit <名字> <秒>：等进程退出，退出码放进 exit_code；超时失败。不能放进 $(...)：子 shell 不是进程的父进程，
# wait 拿不到退出码。
exit_code=0
wait_exit() {
	local name="$1" limit="$2" pid i=0
	pid=$(pid_of "$name")
	while kill -0 "$pid" 2>/dev/null; do
		[ "$i" -lt "$limit" ] || { tail -20 "$work_dir/$name.log" >&2; drill_fail "$name still running after ${limit}s"; }
		i=$((i + 1))
		sleep 1
	done
	exit_code=0
	wait "$pid" 2>/dev/null || exit_code=$?
	mv "${pids_dir:?}/${name:?}" "${pids_dir:?}/${name:?}.exited"
}

require_log() {
	grep -q "$2" "$work_dir/$1.log" || { tail -30 "$work_dir/$1.log" >&2; drill_fail "$1: log has no \"$2\""; }
}

{ go version; git -C "$repo_dir" rev-parse HEAD; printf 'project=%s port_shift=%s etcd_port=%s\n' "$project" "$port_shift" "$etcd_port"; } >"$out/env.txt"
drill_check_ports
drill_start_etcd
(cd "$repo_dir" && go build -o "$out/roost" ./codegen/cmd/roost)
"$out/roost" project new "$project" -module "example.com/$project" -out "$work_dir/$project" -template game --skip-deps >"$out/generate.log" 2>&1
cd "$work_dir/$project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_dir"
go mod tidy >"$out/tidy.log" 2>&1
mkdir -p bin
go build -o bin/app . || drill_fail "build"
grep -q 'a.Singleton(kitetcd.SingletonStore)' internal/bootstrap/generated.go || drill_fail "bootstrap does not install kitetcd.SingletonStore"
drill_configure

# A 的配置：生成的 game 配置（锁打开）；startup_wait 放宽到 60s，让 B 在 A 被冻结后等得到过期。
config_a=configs/service/config.game.yaml
[ "$(yaml_value "$config_a" singleton enabled)" = true ] || drill_fail "game singleton.enabled is not true"
drill_edit -e 's#^  startup_wait: [0-9]*s$#  startup_wait: 60s#' "$config_a"
[ "$(yaml_value "$config_a" singleton startup_wait)" = 60s ] || drill_fail "singleton.startup_wait not raised"
key_prefix="$(yaml_value "$config_a" singleton key_prefix)"
ttl_s="$(yaml_value "$config_a" singleton ttl | tr -d s)"
renew_s="$(yaml_value "$config_a" singleton renew_interval | tr -d s)"
[[ "$key_prefix" == "/$project/singleton" ]] || drill_fail "singleton.key_prefix is $key_prefix, want /$project/singleton"
"$out/roost" project doctor >"$out/doctor.log" 2>&1 || true
grep -E '^OK +singleton:etcd-consistent' "$out/doctor.log" || { grep -E 'singleton:' "$out/doctor.log"; drill_fail "doctor singleton:etcd-consistent is not OK"; }

# B 的配置：同一个 sid，本进程的监听端口（addr: 行，redis: 段的地址除外——那是依赖）与发现广告地址各加 1000；
# 工作目录另起，WAL 不与 A 共用。
config_b="$work_dir/$project/configs/service/config.game-b.yaml"
awk '/^[a-z_]+:/{section=$1}
	section != "redis:" && /^ +(addr|advertise_addr): (127\.0\.0\.1|0\.0\.0\.0):[0-9]+$/ { n = split($0, parts, ":"); sub(/[0-9]+$/, parts[n] + 1000) }
	{ print }' "$config_a" >"$config_b"
diff "$config_a" "$config_b" >"$out/config-b.diff" || true
grep -q "^> .*addr: .*$(($(drill_port 9100) + 1000))\$" "$out/config-b.diff" || drill_fail "B's ops port was not moved (see $out/config-b.diff)"
while read -r port; do
	if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then drill_fail "port $port for the second process is in use"; fi
done < <(grep -E '^> +addr: ' "$out/config-b.diff" | grep -oE '[0-9]+$')
dir_b="$work_dir/b"
mkdir -p "$dir_b"
ln -s "$work_dir/$project/configs" "$dir_b/configs"
ops_a="$(yaml_value "$config_a" ops addr)"
ops_b="$(yaml_value "$config_b" ops addr)"
[[ "$ops_a" != "$ops_b" ]] || drill_fail "the two processes share ops $ops_a"
drill_log "lock key $key_prefix:game:$sid, ttl ${ttl_s}s, renew ${renew_s}s; A ops $ops_a, B ops $ops_b"

# 1. A 持锁就绪；B 阻塞在启动等待。
start_game lock-a "$work_dir/$project" "$work_dir/$project/$config_a"
wait_ready lock-a "$ops_a" 120 >/dev/null
require_log lock-a 'singleton: acquired'
start_game lock-b "$dir_b" "$config_b"
sleep $((renew_s * 3))
kill -0 "$(pid_of lock-b)" 2>/dev/null || { tail -20 "$work_dir/lock-b.log"; drill_fail "B exited while A holds the lock"; }
require_log lock-b 'singleton: waiting for the current holder'
if grep -q 'singleton: acquired' "$work_dir/lock-b.log"; then drill_fail "B acquired the lock while A holds it"; fi
if ready "$ops_b"; then drill_fail "B is ready while A holds the lock"; fi
ready "$ops_a" || drill_fail "A stopped being ready"
drill_log "step 1 PASS: B waits at start-up while A holds the lock"

# 2. 冻结 A：租约过期后 B 接管。
kill -STOP "$(pid_of lock-a)"
drill_log "A frozen (SIGSTOP)"
took="$(wait_ready lock-b "$ops_b" 60)"
require_log lock-b 'singleton: acquired'
[ "$took" -ge $((ttl_s - renew_s - 1)) ] || drill_fail "B took over ${took}s after A froze, before A's lease (ttl ${ttl_s}s, renewed every ${renew_s}s) could have expired"
drill_log "step 2 PASS: B took over ${took}s after A froze"

# 3. 恢复 A：fail-stop，B 照常。
kill -CONT "$(pid_of lock-a)"
drill_log "A resumed (SIGCONT)"
wait_exit lock-a 60
code=$exit_code
[ "$code" -ne 0 ] || drill_fail "A exited 0 after losing the lock; fail-stop must exit non-zero"
require_log lock-a 'singleton lock lost; fail-stop'
if grep -q 'singleton: released' "$work_dir/lock-a.log"; then drill_fail "A released a lock it had lost"; fi
ready "$ops_b" || drill_fail "B stopped being ready after A resumed"
drill_log "step 3 PASS: A failed stop (exit $code) with the lost-lock log; B still serving"

# 4. etcd 停 20s：持锁的 B fail-stop（在窗口内，不等 etcd 恢复）；恢复后同 sid 重新拉起能拿到锁。
kill "$etcd_pid"
wait "$etcd_pid" 2>/dev/null || true
etcd_pid=""
drill_log "etcd stopped"
stopped_at=$(date +%s)
wait_exit lock-b 20
code=$exit_code
lost_after=$(($(date +%s) - stopped_at))
[ "$code" -ne 0 ] || drill_fail "B exited 0 when etcd went away; fail-stop must exit non-zero"
require_log lock-b 'singleton lock lost; fail-stop'
drill_log "B failed stop ${lost_after}s after etcd stopped (exit $code)"
if [ "$lost_after" -lt 20 ]; then sleep $((20 - lost_after)); fi
drill_start_etcd
drill_log "etcd restarted on the same data directory"
start_game lock-c "$work_dir/$project" "$work_dir/$project/$config_a"
took="$(wait_ready lock-c "$ops_a" 120)"
require_log lock-c 'singleton: acquired'
drill_log "step 4 PASS: B failed stop while etcd was down; the same sid started again ${took}s after etcd came back"

for name in lock-a lock-b lock-c; do
	n=$(grep -c '"level":"ERROR"' "$work_dir/$name.log" || true)
	drill_log "$name: $n ERROR log lines"
done
drill_log "PASS singleton etcd drill"
