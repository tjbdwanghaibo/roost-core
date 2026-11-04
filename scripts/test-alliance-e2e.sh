#!/usr/bin/env bash
# 联盟服务阶段 5：真实依赖端到端（docs/maintenance/FEATURE-2026-11-04-alliance-service.md §7 阶段 5）。
#
# 生成当前检出的 game-demo，编译二进制与 accountctl，把 scripts/testdata/alliance-e2e/*_test.go.txt 复制进生成工程的
# e2e/ 后运行。用例用生成的二进制起真实进程（alliance 两个实例、game 两个进程与 game 依赖的九个托管服务），经生成的
# 路由 BusClient、玩家 TCP、运维 /admin 命令驱动，kill -9 / SIGSTOP 造故障，toxiproxy 断 Redis：
#   - TestAllianceE2EPreloadOrder              预加载完成前不接路由 RPC（ALLIANCE_E2E_PRELOAD 个联盟，缺省 500）；
#   - TestAllianceE2ECrashTakeoverAndScaleIn   kill -9 时在途请求收敛、登记 TTL 过期与同 sid 拉起、transfer 改投、缩容三步；
#   - TestAllianceE2EOneAllianceUnderRace      一人一盟：守卫与联盟分属两个实例时并发入会 / 批准，kill -9 任一方后收尾；
#   - TestAllianceE2ERedisOutage               Redis（经 toxiproxy）断开期间路由回退所有权记录的点读放大、恢复后重新登记；
#   - TestAllianceE2EGamePlayers               两个 game 进程上的玩家同一公会互相读到、通知跨进程、Saga 只扣一次只退一次；
#   - TestAllianceE2ERejectedFoundingAfterRestart  建盟被拒留下的空认领：重启后预加载计 empty、不报错，Info 答确定的“不存在”；
#   - TestAllianceE2EShortPerf                 只在 ALLIANCE_E2E_PERF=1 时跑：续期量、预加载 1k / 10k、路由 RPC 延迟；
#   - TestAllianceE2EMirrorRevalidation        只在 ALLIANCE_E2E_PERF=1 时跑：cached_max_staleness 30s / 5s 的 Mirror 回源量。
#   - TestAllianceE2ERPCConcurrency            只在 ALLIANCE_E2E_PERF=1 时跑：一个实例上 Found / Info / Join 的吞吐随并发与顺序键的变化（约 2 分钟）。
# 依赖：scripts/mirror-local.sh 起的私有环境（需要 toxiproxy；会断开它的 redis 代理，不要指向共享隔离环境），
# 加上本脚本自起的私有 etcd（ALLIANCE_E2E_ETCD_PORT，缺省 34379）；进程端口从 ALLIANCE_E2E_PORT_BASE（缺省 35100）起
# 约占 0..300。用例各用唯一前缀并在结束时删掉自己的库 / 流 / 键。耗时约 7 分钟（本机实测 400s，含生成与编译），不进 pretag。
#   ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=32000 scripts/mirror-local.sh up
#   source <私有目录>/roost-dataengine-it/env.sh && scripts/test-alliance-e2e.sh
#   ALLIANCE_E2E_RUN='^TestAllianceE2EGamePlayers$'  只跑某一条；ALLIANCE_E2E_LOGS=<目录> 保留进程日志
#   ALLIANCE_E2E_PERF=1 ALLIANCE_E2E_RUN='^TestAllianceE2E(ShortPerf|MirrorRevalidation)$'  短测（≤30 分钟）
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?isolated Redis required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?isolated NATS required}"
: "${ROOST_DATAENGINE_IT_REDIS_PROXIED_ADDR:?toxiproxy redis proxy required (scripts/mirror-local.sh up)}"
: "${ROOST_DATAENGINE_IT_TOXIPROXY_URL:?toxiproxy required (scripts/mirror-local.sh up)}"
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-alliance-e2e.XXXXXX")"
etcd_port="${ALLIANCE_E2E_ETCD_PORT:-34379}"
port_base="${ALLIANCE_E2E_PORT_BASE:-35100}"
etcd_pid=""
cleanup() {
	if [[ -n "$etcd_pid" ]]; then kill "$etcd_pid" 2>/dev/null || true; wait "$etcd_pid" 2>/dev/null || true; fi
	rm -rf "$work_dir"
}
trap cleanup EXIT
for port in "$etcd_port" "$((etcd_port + 1))" $(seq "$port_base" 20 "$((port_base + 300))"); do
	if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
		echo "port $port is in use; set ALLIANCE_E2E_ETCD_PORT / ALLIANCE_E2E_PORT_BASE" >&2
		exit 2
	fi
done
command -v etcd >/dev/null || { echo "etcd binary not found on PATH" >&2; exit 2; }
etcd --name alliance-e2e --data-dir "$work_dir/etcd" \
	--listen-client-urls "http://127.0.0.1:$etcd_port" --advertise-client-urls "http://127.0.0.1:$etcd_port" \
	--listen-peer-urls "http://127.0.0.1:$((etcd_port + 1))" --initial-advertise-peer-urls "http://127.0.0.1:$((etcd_port + 1))" \
	--initial-cluster "alliance-e2e=http://127.0.0.1:$((etcd_port + 1))" >"$work_dir/etcd.log" 2>&1 &
etcd_pid=$!

default_run='^TestAllianceE2E(PreloadOrder|CrashTakeoverAndScaleIn|OneAllianceUnderRace|RedisOutage|GamePlayers|RejectedFoundingAfterRestart)$'
export GOWORK=off
cd "$repo_dir"
go run ./codegen/cmd/roost project new planet -module example.com/planet -out "$work_dir/planet" -template game-demo --skip-deps
printf '\nreplace github.com/tjbdwanghaibo/roost-core => %s\n' "$repo_dir" >> "$work_dir/planet/go.mod"
mkdir -p "$work_dir/planet/e2e"
for source in scripts/testdata/alliance-e2e/*_test.go.txt; do
	cp "$source" "$work_dir/planet/e2e/$(basename "${source%.txt}")"
done
(
	cd "$work_dir/planet"
	go build -mod=mod -o "$work_dir/bin/app" .
	go build -mod=mod -o "$work_dir/bin/accountctl" ./cmd/accountctl
	ALLIANCE_E2E_BINARY="$work_dir/bin/app" ALLIANCE_E2E_ACCOUNTCTL="$work_dir/bin/accountctl" ALLIANCE_E2E_ROOT="$work_dir/planet" \
		ALLIANCE_E2E_ETCD="127.0.0.1:$etcd_port" ALLIANCE_E2E_PORT_BASE="$port_base" \
		go test -mod=mod -count=1 -timeout 60m -v -run "${ALLIANCE_E2E_RUN:-$default_run}" ./e2e/
)
