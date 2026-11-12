#!/usr/bin/env bash
# Run from a generated game-demo project with .acceptance/main.go installed.
# Services must already be ready. Only development credentials are used.
set -euo pipefail
umask 077
portal_url="${PORTAL_URL:-http://127.0.0.1:8081}"
python3 - <<'PY'
import json, time
from pathlib import Path
Path('.acceptance/login-request.json').write_text(json.dumps({
    'channel': 'dev', 'open_id': 'logging-smoke-' + str(time.time_ns()),
    'credential': 'dev-local'}))
PY
curl --fail-with-body --silent --show-error -H 'Content-Type: application/json' \
  --data-binary @.acceptance/login-request.json "$portal_url/portal/login" -o .acceptance/login.json
python3 - <<'PY'
import json
from pathlib import Path
reply = json.loads(Path('.acceptance/login.json').read_text())
assert reply['ok'], reply.get('code')
Path('.acceptance/servers-request.json').write_text(json.dumps({'account_ticket': reply['account_ticket']}))
Path('.acceptance/enter-request.json').write_text(json.dumps({'account_ticket': reply['account_ticket'], 'server_id': 1000}))
PY
curl --fail-with-body --silent --show-error -H 'Content-Type: application/json' \
  --data-binary @.acceptance/servers-request.json "$portal_url/portal/servers" -o .acceptance/servers.json
curl --fail-with-body --silent --show-error -H 'Content-Type: application/json' \
  --data-binary @.acceptance/enter-request.json "$portal_url/portal/enter" -o .acceptance/enter.json
python3 - <<'PY'
import json
from pathlib import Path
servers = json.loads(Path('.acceptance/servers.json').read_text())
entered = json.loads(Path('.acceptance/enter.json').read_text())
assert servers['ok'] and entered['ok']
assert servers['recommended_server_id'] == 1000
assert entered['addr'] == '127.0.0.1:7000'
print('PASS: curl login, servers, enter; server=1000; address=127.0.0.1:7000; tickets redacted')
PY
GOWORK=off go run ./.acceptance "$@"
