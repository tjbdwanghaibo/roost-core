#!/usr/bin/env bash
# P8 GM/purchase：正式 demo、真实 ops HTTP / NATS / 玩家 TCP / Mongo / 文件 WAL。
# Redis 由用例自己启动（AOF always、独占端口与目录），SIGKILL 后沿用原目录；不重启调用方的 Redis。
# source <private mirror-local>/roost-dataengine-it/env.sh
# OPERATIONS_REAL_OUT=/abs/evidence bash scripts/test-operations-real.sh
# OPERATIONS_REAL_MUTATION=purchase-replay 是临时生成物负对照，必须失败；不进 pretag、不测容量。
# 精确用例：TestOperationsGMHTTPMailAndRehome、TestOperationsPurchaseCrashAcrossSIDAndRedisRestart。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo replica set required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${OPERATIONS_REAL_OUT:?absolute evidence directory required}"
[[ "$OPERATIONS_REAL_OUT" == /* ]] || exit 2
case "${OPERATIONS_REAL_MUTATION:-}" in ''|purchase-replay) ;; *) exit 2 ;; esac
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-operations-real.XXXXXX")"
etcd_port="${OPERATIONS_REAL_ETCD_PORT:-57979}"
port_base="${OPERATIONS_REAL_PORT_BASE:-45500}"
etcd_pid=""
cleanup() {
	if [[ -n "$etcd_pid" ]]; then kill "$etcd_pid" 2>/dev/null || true; wait "$etcd_pid" 2>/dev/null || true; fi
	if [[ "${OPERATIONS_REAL_KEEP:-0}" != 1 ]]; then rm -rf "$work_dir"; fi
}
trap cleanup EXIT
mkdir -p "$OPERATIONS_REAL_OUT"
for tool in etcd redis-server; do command -v "$tool" >/dev/null || exit 2; done
for port in "$etcd_port" "$((etcd_port + 1))" $(seq "$port_base" "$((port_base + 350))"); do
	if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then echo "port $port in use" >&2; exit 2; fi
done
export GOWORK=off GOFLAGS="${GOFLAGS:--p=2} -mod=mod"
git -C "$repo_dir" rev-parse HEAD >"$OPERATIONS_REAL_OUT/baseline.txt"
go version >"$OPERATIONS_REAL_OUT/go-version.txt"
etcd --name operations-real --data-dir "$work_dir/etcd" \
	--listen-client-urls "http://127.0.0.1:$etcd_port" --advertise-client-urls "http://127.0.0.1:$etcd_port" \
	--listen-peer-urls "http://127.0.0.1:$((etcd_port + 1))" --initial-advertise-peer-urls "http://127.0.0.1:$((etcd_port + 1))" \
	--initial-cluster "operations-real=http://127.0.0.1:$((etcd_port + 1))" >"$OPERATIONS_REAL_OUT/etcd.log" 2>&1 &
etcd_pid=$!
cd "$repo_dir"
go run ./codegen/cmd/roost project new planet -module example.com/operations-accept -out "$work_dir/planet" -template game-demo --skip-deps >"$OPERATIONS_REAL_OUT/generate.log" 2>&1
printf '\nreplace github.com/tjbdwanghaibo/roost-core => %s\n' "$repo_dir" >>"$work_dir/planet/go.mod"
cd "$work_dir/planet"
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost id check >"$OPERATIONS_REAL_OUT/id-check.log" 2>&1
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost generate --check >"$OPERATIONS_REAL_OUT/generate-check.log" 2>&1
python3 "$repo_dir/scripts/testdata/operations-real/install.py" "$repo_dir" "$work_dir/planet" "${OPERATIONS_REAL_MUTATION:-}"
cd "$work_dir/planet"
go build -o "$work_dir/app" .
go build -o "$work_dir/accountctl" ./cmd/accountctl
go vet ./e2e ./game/controllers/purchase
ALLIANCE_E2E_BINARY="$work_dir/app" ALLIANCE_E2E_ACCOUNTCTL="$work_dir/accountctl" ALLIANCE_E2E_ROOT="$PWD" \
	ALLIANCE_E2E_ETCD="127.0.0.1:$etcd_port" ALLIANCE_E2E_PORT_BASE="$port_base" ALLIANCE_E2E_LOGS="$OPERATIONS_REAL_OUT/processes" \
	go test -race -count=1 -timeout 12m -v -run "${OPERATIONS_REAL_RUN:-^TestOperations(GMHTTPMailAndRehome|PurchaseCrashAcrossSIDAndRedisRestart)$}" ./e2e | tee "$OPERATIONS_REAL_OUT/result.log"
if grep -q -- '--- SKIP:' "$OPERATIONS_REAL_OUT/result.log"; then echo 'unexpected SKIP' >&2; exit 1; fi
[[ $(grep -c '^--- PASS: TestOperations' "$OPERATIONS_REAL_OUT/result.log") == "${OPERATIONS_REAL_EXPECT:-2}" ]]
