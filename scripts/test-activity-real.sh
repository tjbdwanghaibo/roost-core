#!/usr/bin/env bash
# P8 活动：精简 casual 配方 + gm/rehome/alliance/activity_world/activity_alliance。
# 与完整 demo 的 GM/purchase 验收分开；不绕 Player DAO 的 64 顶层字段上限。
# source <private mirror-local>/roost-dataengine-it/env.sh
# ACTIVITY_REAL_OUT=/abs/evidence bash scripts/test-activity-real.sh
# ACTIVITY_REAL_MUTATION=world-driver 为恢复扫描缺失负对照，必须失败。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo replica set required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${ACTIVITY_REAL_OUT:?absolute evidence directory required}"
[[ "$ACTIVITY_REAL_OUT" == /* ]] || exit 2
case "${ACTIVITY_REAL_MUTATION:-}" in ''|world-driver) ;; *) exit 2 ;; esac
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-activity-real.XXXXXX")"
etcd_port="${ACTIVITY_REAL_ETCD_PORT:-58179}"
port_base="${ACTIVITY_REAL_PORT_BASE:-46500}"
etcd_pid=""
cleanup() {
	if [[ -n "$etcd_pid" ]]; then kill "$etcd_pid" 2>/dev/null || true; wait "$etcd_pid" 2>/dev/null || true; fi
	if [[ "${ACTIVITY_REAL_KEEP:-0}" != 1 ]]; then rm -rf "$work_dir"; fi
}
trap cleanup EXIT
mkdir -p "$ACTIVITY_REAL_OUT"
for tool in etcd redis-server; do command -v "$tool" >/dev/null || exit 2; done
for port in "$etcd_port" "$((etcd_port + 1))" $(seq "$port_base" "$((port_base + 350))"); do
	if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then echo "port $port in use" >&2; exit 2; fi
done
export GOWORK=off GOFLAGS="${GOFLAGS:--p=2} -mod=mod"
git -C "$repo_dir" rev-parse HEAD >"$ACTIVITY_REAL_OUT/baseline.txt"
go version >"$ACTIVITY_REAL_OUT/go-version.txt"
etcd --name activity-real --data-dir "$work_dir/etcd" \
	--listen-client-urls "http://127.0.0.1:$etcd_port" --advertise-client-urls "http://127.0.0.1:$etcd_port" \
	--listen-peer-urls "http://127.0.0.1:$((etcd_port + 1))" --initial-advertise-peer-urls "http://127.0.0.1:$((etcd_port + 1))" \
	--initial-cluster "activity-real=http://127.0.0.1:$((etcd_port + 1))" >"$ACTIVITY_REAL_OUT/etcd.log" 2>&1 &
etcd_pid=$!
cd "$repo_dir"
go build -o "$work_dir/roost" ./codegen/cmd/roost
"$work_dir/roost" project new planet -module example.com/activity-accept -out "$work_dir/planet" -recipe casual --skip-deps >"$ACTIVITY_REAL_OUT/generate.log" 2>&1
printf '\nreplace github.com/tjbdwanghaibo/roost-core => %s\n' "$repo_dir" >>"$work_dir/planet/go.mod"
cd "$work_dir/planet"
"$work_dir/roost" module add gm rehome alliance activity_world activity_alliance >"$ACTIVITY_REAL_OUT/modules.log" 2>&1
python3 "$repo_dir/scripts/testdata/activity-real/install.py" "$repo_dir" "$PWD" "${ACTIVITY_REAL_MUTATION:-}"
"$work_dir/roost" generate >"$ACTIVITY_REAL_OUT/generate-tables.log" 2>&1
"$work_dir/roost" id check >"$ACTIVITY_REAL_OUT/id-check.log" 2>&1
"$work_dir/roost" generate --check >"$ACTIVITY_REAL_OUT/generate-check.log" 2>&1
go build -o "$work_dir/app" ./cmd/activityaccept
go build -o "$work_dir/accountctl" ./cmd/accountctl
go vet ./e2e ./cmd/activityaccept
ALLIANCE_E2E_BINARY="$work_dir/app" ALLIANCE_E2E_ACCOUNTCTL="$work_dir/accountctl" ALLIANCE_E2E_ROOT="$PWD" \
	ALLIANCE_E2E_ETCD="127.0.0.1:$etcd_port" ALLIANCE_E2E_PORT_BASE="$port_base" ALLIANCE_E2E_LOGS="$ACTIVITY_REAL_OUT/processes" \
	go test -race -count=1 -timeout 8m -v -run '^TestOperationsActivityAuthorityMigration$' ./e2e | tee "$ACTIVITY_REAL_OUT/result.log"
if grep -q -- '--- SKIP:' "$ACTIVITY_REAL_OUT/result.log"; then echo 'unexpected SKIP' >&2; exit 1; fi
[[ $(grep -c '^--- PASS: TestOperationsActivityAuthorityMigration' "$ACTIVITY_REAL_OUT/result.log") == 1 ]]
