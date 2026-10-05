#!/usr/bin/env bash
# NC-203 控制：持锁者导出 ROOST_REMOTE_ACCEPTANCE_LOCK_HELD 后，自己的子入口照常执行；
# acquire_acceptance_lock 在锁空闲时取得、退出时释放，锁被占时拒绝。临时根，无进程。
set -uo pipefail
src="$1"
home="$(mktemp -d "${TMPDIR:-/tmp}/n15-lock.XXXXXX")"; home="$(cd "$home" && pwd -P)"
trap 'rm -rf "$home"' EXIT
root="$home/roost-dataengine-it"; lock="$root/remote-acceptance.lock"
mkdir -p "$lock"; printf '31000\n' > "$root/port-offset"
e=(env ROOST_IT_HOME="$home" ROOST_IT_PORT_OFFSET=31000 ROOST_DATAENGINE_IT_ROOT="$root" ROOST_DATAENGINE_IT=1 ROOST_REMOTE_ACCEPTANCE_LOCK_HELD="$lock")
"${e[@]}" bash "$src/kit/scripts/integration/dataengine-env.sh" fault nats-all >/dev/null 2>&1; echo "held: fault nats-all exit=$?"
"${e[@]}" bash "$src/scripts/remote-fault.sh" nats-all >/dev/null 2>&1; echo "held: remote-fault nats-all exit=$?"
"${e[@]}" bash "$src/kit/scripts/integration/dataengine-env.sh" down >/dev/null 2>&1; echo "held: down exit=$?"
rmdir "$lock"
lib="$src/kit/scripts/integration/lib"
env -u ROOST_REMOTE_ACCEPTANCE_LOCK_HELD ROOST_IT_HOME="$home" ROOST_IT_PORT_OFFSET=31000 ROOST_DATAENGINE_IT_ROOT="$root" bash -c '
source "$0/common.sh"
acquire_acceptance_lock; echo "acquire(free) exit=$? held=${ROOST_REMOTE_ACCEPTANCE_LOCK_HELD:+yes} lockdir=$([[ -d "$ROOST_IT_ACCEPTANCE_LOCK" ]] && echo present)"
env -u ROOST_REMOTE_ACCEPTANCE_LOCK_HELD bash -c "source \"$0/common.sh\"; acquire_acceptance_lock; echo \"second acquire exit=\$?\"" 2>&1 | tail -1
' "$lib"
[[ -d "$lock" ]] && echo "after exit: lock still present" || echo "after exit: lock released"
