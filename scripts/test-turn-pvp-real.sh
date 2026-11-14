#!/usr/bin/env bash
# 正式 turn-pvp 配方的真实登录 / 双人匹配 / Room / Turn / 独立 Gate / SIGKILL 重启验收。
# 私有 Mongo 副本集、Redis、NATS 由调用方提供；etcd 与全部业务进程由本脚本自起、只清理本次工程。
# 入口：ROOST_TURN_PVP_REAL=1 bash scripts/test-turn-pvp-generated.sh（也可单独运行本脚本）。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo replica-set URI required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?private Redis required}"
for tool in etcd jq curl mongosh redis-cli nats; do command -v "$tool" >/dev/null || { echo "$tool not found" >&2; exit 2; }; done
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-turn-real.XXXXXX")"
project="$(printf 'trn%06x' $(((RANDOM << 9) ^ RANDOM)))"
out="${ROOST_TURN_PVP_OUT:-$repo_dir/artifacts/turn-pvp-real-$project}"
port_shift="${ROOST_TURN_PVP_PORT_SHIFT:-35000}"
etcd_port="${ROOST_TURN_PVP_ETCD_PORT:-57779}"
etcd_pid=""
mkdir -p "$out"
export GOWORK=off GOFLAGS="${GOFLAGS:-} -mod=mod" ROOST_TURN_PVP_REAL=1
source "$repo_dir/scripts/testdata/demo-drill/lib.sh"
trap drill_cleanup EXIT
drill_check_ports
drill_start_etcd
drill_log "generating turn-pvp recipe $project"
(cd "$repo_dir" && go run ./codegen/cmd/roost project new "$project" -module "example.com/$project" -out "$work_dir/$project" -recipe turn-pvp --skip-deps) >"$out/generate.log" 2>&1
cd "$work_dir/$project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_dir"
for name in gate ingress; do
	mkdir -p "cmd/drill$name"
	sed "s|DRILL_MODULE|example.com/$project|g" "$repo_dir/scripts/testdata/demo-drill/$name.go.txt" >"cmd/drill$name/main.go"
done
mkdir -p .acceptance bin
sed "s|TURN_MODULE|example.com/$project|g" "$repo_dir/scripts/testdata/turn-pvp-real/recipe_test.go.txt" >.acceptance/recipe_test.go
gofmt -w .acceptance/recipe_test.go
go build -o bin/app .
go build -o bin/accountctl ./cmd/accountctl
go build -o bin/drillgate ./cmd/drillgate
go build -o bin/drillingress ./cmd/drillingress
go test -c -o bin/turnprobe ./.acceptance
drill_configure
make dev-run >"$out/dev-run.log" 2>&1 || { tail -25 "$out/dev-run.log"; drill_fail "dev-run"; }
sh deploy/dev/run.sh stop game >/dev/null
drill_ingress_config configs/service/config.game.yaml configs/service/config.game-ingress.yaml
# 配方只有 account / game / match / rank 等服务，直接从 game 取 Gate 所需公共配置。
awk '/^[a-z_]+:/{keep=($0 ~ /^(sid|log|redis|nats|etcd|ops|singleton):/)} keep' configs/service/config.game.yaml >configs/service/config.gate.yaml
drill_edit -e "s#^  addr: 127.0.0.1:$(drill_port 9100)\$#  addr: 127.0.0.1:$(drill_port 9300)#" \
	-e "s#^  advertise_addr: .*#  advertise_addr: 127.0.0.1:$(drill_port 7100)#" \
	-e 's#^sid: .*#sid: 9500#' configs/service/config.gate.yaml
awk '/^singleton:/{s=1} s&&/^  enabled: false$/{sub(/false/, "true"); s=0} {print}' configs/service/config.gate.yaml >configs/service/config.gate.tmp
mv configs/service/config.gate.tmp configs/service/config.gate.yaml
drill_start game-ingress bin/drillingress game 1000 configs/service/config.game-ingress.yaml
drill_start gate bin/drillgate gate 9500 configs/service/config.gate.yaml \
	DRILL_GATE_ADDR="127.0.0.1:$(drill_port 7100)" DRILL_GATE_ADVERTISE="127.0.0.1:$(drill_port 7100)"
export TURN_REAL_PROJECT="$project" TURN_REAL_PORTAL="http://127.0.0.1:$(drill_port 8081)" \
	TURN_REAL_ENDPOINT="127.0.0.1:$(drill_port 7100)" TURN_REAL_STATE="$work_dir/turn-state.json"
probe() {
	TURN_REAL_PHASE="$1" bin/turnprobe -test.run '^TestTurnRecipeRealLifecycle$' -test.v -test.timeout 2m >"$out/recipe-$1.log" 2>&1 || {
		cat "$out/recipe-$1.log"; drill_fail "turn recipe $1";
	}
	grep -q '^--- PASS: TestTurnRecipeRealLifecycle' "$out/recipe-$1.log" || drill_fail "probe did not run"
	grep -q -- '--- SKIP:' "$out/recipe-$1.log" && drill_fail "unexpected skipped probe"
	return 0
}
probe seed
pid=$(cat .dev/game-ingress.pid)
kill -9 "$pid"
wait "$pid" 2>/dev/null || true
rm .dev/game-ingress.pid
drill_log "SIGKILL game-ingress; waiting past two saved turn deadlines"
# 以确认的服务端期限为屏障，让离线时间跨过多个回合，恢复仍只能补推进一次。
python3 - "$TURN_REAL_STATE" <<'PY'
import json,sys,time
deadline=json.load(open(sys.argv[1]))['Deadline']/1000+10
while time.time()<deadline:
    time.sleep(min(.1,deadline-time.time()))
PY
drill_start game-ingress bin/drillingress game 1000 configs/service/config.game-ingress.yaml
probe recover
cat "$out/recipe-seed.log" "$out/recipe-recover.log" | grep 'PASS real turn'
drill_log "PASS real turn-pvp recipe: Gate/TCP/NATS/Mongo/WAL, SIGKILL recovery, offline timeout once and replay; evidence $out"
