#!/usr/bin/env bash
# Generated Player endpoint -> embedded TCP -> Nest -> file WAL -> Mongo.
# Functional smoke: ROOST_GENERATED_TCP_SECONDS=10 CONNECTIONS=50 ENTITY_MODE=distinct|shared;
# SELF_SYNC=0|1 DURABILITY=strict|pipelined|async OPEN_LOOP_RATE=0|requests_per_second.
# A positive rate uses scheduled arrivals with a bounded queue; late/drop counts are explicit.
# Requires an isolated ROOST_DATAENGINE_IT_MONGO_URI. Retains the consumer, WAL, reports and logs.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
output="${ROOST_GENERATED_TCP_OUTPUT:-$(mktemp -d "${TMPDIR:-/tmp}/roost-generated-tcp.XXXXXX")}"
mkdir -p "$output"
output="$(cd "$output" && pwd)"
[[ ! -e "$output/project" ]] || { echo "project already exists: $output/project" >&2; exit 2; }
: "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica set required}"
export GOWORK=off GOFLAGS="${GOFLAGS:--p=2}"
export DURABILITY="${DURABILITY:-strict}" SELF_SYNC="${SELF_SYNC:-0}" CONNECTIONS="${CONNECTIONS:-50}" ENTITY_MODE="${ENTITY_MODE:-distinct}" OPEN_LOOP_RATE="${OPEN_LOOP_RATE:-0}"
export ROOST_GENERATED_TCP_SECONDS="${ROOST_GENERATED_TCP_SECONDS:-10}"
case "$DURABILITY" in strict|pipelined|async);; *) echo 'invalid DURABILITY' >&2; exit 2;; esac
case "$SELF_SYNC" in 0|1);; *) echo 'invalid SELF_SYNC' >&2; exit 2;; esac
case "$ENTITY_MODE" in distinct|shared);; *) echo 'invalid ENTITY_MODE' >&2; exit 2;; esac
[[ "$CONNECTIONS" =~ ^[1-9][0-9]*$ && "$OPEN_LOOP_RATE" =~ ^[0-9]+$ && "$ROOST_GENERATED_TCP_SECONDS" =~ ^[1-9][0-9]*$ ]] || { echo 'invalid numeric settings' >&2; exit 2; }
export ROOST_GENERATED_TCP_REPORT="$output/report.json" ROOST_GENERATED_TCP_WAL="$output/wal"
export ROOST_GENERATED_TCP_DB="roost_generated_tcp_${PPID}_$$_$(date +%s)"
{
 go version
 git -C "$repo_root" rev-parse HEAD
 git -C "$repo_root" status --short
 printf 'database=%s\n' "$ROOST_GENERATED_TCP_DB"
 printf 'durability=%s self_sync=%s connections=%s entities=%s seconds=%s open_loop_rate=%s\n' "$DURABILITY" "$SELF_SYNC" "$CONNECTIONS" "$ENTITY_MODE" "$ROOST_GENERATED_TCP_SECONDS" "$OPEN_LOOP_RATE"
 if command -v sysctl >/dev/null; then sysctl -n vm.loadavg 2>/dev/null || true; sysctl vm.swapusage 2>/dev/null || true; fi
} > "$output/env.txt"
cd "$repo_root"
go build -o "$output/roost" ./codegen/cmd/roost
"$output/roost" project new tcp-perf -module example.com/tcp-perf -out "$output/project" -template game -mods configdata,mongo,nats,dataengine,nest -skip-deps > "$output/generate.log" 2>&1
cd "$output/project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
run_roost() { "$output/roost" "$@" >> "$output/generate.log" 2>&1; }
run_roost add access player --service game
run_roost add dao Player --entity Player
run_roost add component Wallet --entity Player
run_roost add protocol AddGold --group game --handler player
run_roost add protocol EnterGame --group game --handler player
run_roost add transport tcp
cp "$repo_root/scripts/testdata/generated-tcp/player.go.txt" db/def/player.go
cp "$repo_root/scripts/testdata/generated-tcp/add_gold.go.txt" game/handler/add_gold.go
cp "$repo_root/scripts/testdata/generated-tcp/wallet.go.txt" game/entities/player/wallet.go
cp "$repo_root/scripts/testdata/generated-tcp/auth.go.txt" internal/access/player/tcp/auth.go
cp "$repo_root/scripts/testdata/generated-tcp/controller.go.txt" game/controllers/player/controller.go
cp "$repo_root/scripts/testdata/generated-tcp/enter_game.go.txt" game/controllers/player/enter_game.go
python3 - "$DURABILITY" <<'PY'
from pathlib import Path
import sys
p=Path('game/handler/add_gold.go');p.write_text(p.read_text().replace('durability=strict','durability='+sys.argv[1]))
p=Path('protocol/def/add_gold.go');p.write_text(p.read_text().replace('type AddGoldResponse struct {','type AddGoldResponse struct {\n Gold int64 `pb:"3"`'))
p=Path('roost.yaml');s=p.read_text();old='        transports:\n            - tcp\n';assert old in s;p.write_text(s.replace(old,old+'        self_sync: true\n'))
PY
run_roost add endpoint AddGold --handler player
run_roost project sync --skip-deps
run_roost generate
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
cp "$repo_root/scripts/testdata/generated-tcp/load_test.go.txt" internal/bootstrap/generated_tcp_test.go
gofmt -w db/def/player.go game/handler/add_gold.go game/controllers/player/*.go internal/access/player/tcp/auth.go internal/bootstrap/generated_tcp_test.go
go mod tidy > "$output/dependencies.log" 2>&1
run_roost id check
run_roost generate --check
go build ./...
go vet ./...
go test -c -o "$output/generated-tcp.test" ./internal/bootstrap
"$output/generated-tcp.test" -test.run '^TestGeneratedTCPPersistentLoad$' -test.v -test.timeout=10m | tee "$output/test.log"
if [[ "$(grep -c '^--- PASS: TestGeneratedTCPPersistentLoad' "$output/test.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$output/test.log"; then exit 1; fi
echo "Generated TCP artifacts: $output"
