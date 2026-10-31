#!/usr/bin/env bash
# Run the generated game-demo with an isolated local NATS and the C# GameClient.
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"
tools=(go nats-server)
if [[ "${ROOST_GATE_CPP:-}" == "1" ]]; then tools+=(cmake); else tools+=(dotnet); fi
for tool in "${tools[@]}"; do
  command -v "$tool" >/dev/null || { echo "Required tool not found: $tool" >&2; exit 1; }
done
run_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-gate-client.XXXXXX")"
nats_pid=""
cleanup() {
  if [[ -n "$nats_pid" ]]; then kill "$nats_pid" 2>/dev/null || true; wait "$nats_pid" 2>/dev/null || true; fi
  rm -rf "$run_dir"
}
trap cleanup EXIT
nats-server -a 127.0.0.1 -p -1 > "$run_dir/nats.log" 2>&1 &
nats_pid=$!
endpoint=""
for ((attempt=0; attempt<100; attempt++)); do
  endpoint="$(sed -n 's/.*Listening for client connections on \(127\.0\.0\.1:[0-9]*\).*/\1/p' "$run_dir/nats.log" | head -n 1)"
  [[ -n "$endpoint" ]] && break
  kill -0 "$nats_pid" 2>/dev/null || { cat "$run_dir/nats.log" >&2; exit 1; }
  sleep 0.1
done
[[ -n "$endpoint" ]] || { cat "$run_dir/nats.log" >&2; exit 1; }
export ROOST_GATE_DOTNET=0 ROOST_DATAENGINE_IT_NATS_URL="nats://$endpoint"
if [[ "${ROOST_GATE_CPP:-}" != "1" ]]; then export ROOST_GATE_DOTNET=1; fi
GOWORK=off go test -count=1 -v -timeout 10m -run '^TestGateGeneratedBusinessConsumer$' ./codegen/internal/roost
