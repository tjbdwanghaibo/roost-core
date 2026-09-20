#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
script="$repo_root/scripts/integration/dataengine-env.sh"

if [[ ! -x "$script" ]]; then
	echo "dataengine environment script is missing or not executable: $script" >&2
	exit 1
fi

status_code=0
"$script" status >/dev/null 2>&1 || status_code=$?
if [[ "$status_code" -ne 0 && "$status_code" -ne 1 ]]; then
	echo "status returned unexpected code $status_code" >&2
	exit 1
fi

unsafe_output="$(ROOST_DATAENGINE_IT_ROOT=/tmp/not-roost "$script" reset 2>&1 || true)"
if [[ "$unsafe_output" != *"refuse unsafe root"* ]]; then
	echo "reset did not reject unsafe root: $unsafe_output" >&2
	exit 1
fi

echo "dataengine environment shell tests passed"
