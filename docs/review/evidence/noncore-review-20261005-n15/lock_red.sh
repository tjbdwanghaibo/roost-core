#!/usr/bin/env bash
# NC-203：remote-acceptance.lock 被别的验收持有时，会改动环境的入口必须拒绝。
# 只用临时根（端口偏移 31000，没有任何进程），不碰共享隔离环境。
set -uo pipefail
src="$1"
home="$(mktemp -d "${TMPDIR:-/tmp}/n15-lock.XXXXXX")"; home="$(cd "$home" && pwd -P)"
trap 'rm -rf "$home"' EXIT
root="$home/roost-dataengine-it"
mkdir -p "$root/remote-acceptance.lock"
printf '31000\n' > "$root/port-offset"
run() {
  local code=0 out
  out="$(env -u ROOST_REMOTE_ACCEPTANCE_LOCK_HELD ROOST_IT_HOME="$home" ROOST_IT_PORT_OFFSET=31000 ROOST_DATAENGINE_IT_ROOT="$root" ROOST_DATAENGINE_IT=1 "$@" 2>&1)" || code=$?
  printf '%-40s exit=%s  %s\n' "$*" "$code" "$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-140)"
}
run bash "$src/kit/scripts/integration/dataengine-env.sh" fault nats-all
run bash "$src/kit/scripts/integration/dataengine-env.sh" down
run bash "$src/scripts/remote-fault.sh" nats-all
# reset 会 rm -rf 整个根（含锁目录）：放最后，看锁是否还在
run bash "$src/kit/scripts/integration/dataengine-env.sh" reset
if [[ -d "$root/remote-acceptance.lock" ]]; then echo "RESULT: lock dir still present"; else echo "RESULT: lock dir (and root) REMOVED by reset while held"; fi
# status 只读，持锁期间照常
run bash "$src/kit/scripts/integration/dataengine-env.sh" status
