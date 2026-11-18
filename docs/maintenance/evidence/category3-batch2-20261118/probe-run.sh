#!/usr/bin/env bash
# 历史探针 TestJoinScaleRetry（alltimeout-20261106/probe 的副本）在当前树上复跑：私有 mirror-local（偏移 21000），
# 单个 alliance 实例，级别 128,256,256，每段 12s，停顿监视器（≥32 个尝试已超过 2s 时抓栈）。
# 用法：probe-run.sh <输出目录> <轮数> <bg 值列表>
set -uo pipefail
repo=/Users/whb/roost/roost-core/.claude/worktrees/agent-acb3c435f4be2cf12
probe=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/c3b2/probe
out="$1"; rounds="$2"; bgs="$3"
mkdir -p "$out"
export ROOST_MIRROR_LOCAL_HOME="$out/mirror" ROOST_MIRROR_LOCAL_OFFSET=21000 GOWORK=off
host() { date -u '+%Y-%m-%dT%H:%M:%SZ'; uptime; sysctl vm.swapusage; df -h / | tail -1; }
work="$out/work"
etcd_port=34379; port_base=35100; pp=127.0.0.1:35099
etcd_pid=""
cleanup() {
  [[ -n "$etcd_pid" ]] && { kill "$etcd_pid" 2>/dev/null; wait "$etcd_pid" 2>/dev/null; }
  bash "$repo/scripts/mirror-local.sh" clean >>"$out/mirror-clean.log" 2>&1
}
trap cleanup EXIT
host >"$out/host-start.txt"
# 先编译（不在计时段内）
mkdir -p "$work"
(cd "$repo" && go run ./codegen/cmd/roost project new planet -module example.com/planet -out "$work/planet" -template game-demo --skip-deps) >"$out/generate.log" 2>&1 || { echo "generate failed"; exit 1; }
printf '\nreplace github.com/tjbdwanghaibo/roost-core => %s\n' "$repo" >>"$work/planet/go.mod"
cp "$probe/pprof_probe.go.txt" "$work/planet/pprof_probe.go"
mkdir -p "$work/planet/e2e"
for source in "$repo"/scripts/testdata/alliance-e2e/*_test.go.txt; do cp "$source" "$work/planet/e2e/$(basename "${source%.txt}")"; done
cp "$probe/joinprobe_test.go.txt" "$work/planet/e2e/joinprobe_test.go"
cp "$probe/joinretry_test.go.txt" "$work/planet/e2e/joinretry_test.go"
(cd "$work/planet" && go build -mod=mod -o "$work/bin/app" . && go build -mod=mod -o "$work/bin/accountctl" ./cmd/accountctl && go test -mod=mod -c -o "$work/bin/e2e.test" ./e2e/) >"$out/build.log" 2>&1 || { echo "build failed"; tail -20 "$out/build.log"; exit 1; }
bash "$repo/scripts/mirror-local.sh" up >"$out/mirror-up.log" 2>&1 || { echo "mirror up failed"; exit 1; }
set -a; source "$out/mirror/roost-dataengine-it/env.sh"; set +a
for r in $(seq 1 "$rounds"); do
  for bg in $bgs; do
    label="r$r-bg$bg"; d="$out/$label"; mkdir -p "$d/logs"
    rm -rf "$work/etcd"
    etcd --name joinscale --data-dir "$work/etcd" \
      --listen-client-urls "http://127.0.0.1:$etcd_port" --advertise-client-urls "http://127.0.0.1:$etcd_port" \
      --listen-peer-urls "http://127.0.0.1:$((etcd_port + 1))" --initial-advertise-peer-urls "http://127.0.0.1:$((etcd_port + 1))" \
      --initial-cluster "joinscale=http://127.0.0.1:$((etcd_port + 1))" >"$d/etcd.log" 2>&1 &
    etcd_pid=$!
    host >"$d/host-before.txt"
    (cd "$work/planet/e2e" && PLANET_PPROF_ADDR=$pp JOINPROBE_OUT="$d" ALLIANCE_E2E_PERF=1 ALLIANCE_E2E_LOGS="$d/logs" \
      ALLIANCE_E2E_BINARY="$work/bin/app" ALLIANCE_E2E_ACCOUNTCTL="$work/bin/accountctl" ALLIANCE_E2E_ROOT="$work/planet" \
      ALLIANCE_E2E_ETCD="127.0.0.1:$etcd_port" ALLIANCE_E2E_PORT_BASE="$port_base" \
      JOINSCALE_LEVELS=128,256,256 JOINSCALE_SECONDS=12 JOINSCALE_KEYS="dataengine.projection.background_checkpoint=$([[ $bg == 1 ]] && echo true || echo false)" \
      "$work/bin/e2e.test" -test.count=1 -test.timeout=30m -test.v -test.run '^TestJoinScaleRetry$') >"$d/probe.log" 2>&1
    echo "$label exit=$?" | tee -a "$out/runs.txt"
    host >"$d/host-after.txt"
    kill "$etcd_pid" 2>/dev/null; wait "$etcd_pid" 2>/dev/null; etcd_pid=""
  done
done
host >"$out/host-end.txt"
echo PROBE-DONE
