#!/usr/bin/env bash
# Real account RPC/Redis -> Gate/NATS -> generated business/WAL/Mongo -> C# client.
# Use the isolated integration environment. This script never starts/stops shared services.
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?isolated Redis required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?isolated NATS required}"
command -v dotnet >/dev/null || { echo "Required .NET SDK not found" >&2; exit 1; }
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"
log="$(mktemp "${TMPDIR:-/tmp}/roost-gate-real.XXXXXX")"
echo "Real Gate evidence: $log"
ROOST_GATE_REAL=1 ROOST_GATE_DOTNET=1 GOWORK=off go test -count=1 -v -timeout 10m \
  -run '^TestGateGeneratedBusinessConsumer$' ./codegen/internal/roost | tee "$log"
grep -q 'PASS real Gate:' "$log"
grep -q 'PASS C# Gate/PB/Nest/DAO/Sync' "$log"
grep -q '^--- PASS: TestGateGeneratedBusinessConsumer' "$log"
if grep -q -- '--- SKIP:' "$log"; then echo "Unexpected skipped acceptance" >&2; exit 1; fi
