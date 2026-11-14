#!/usr/bin/env bash
set -euo pipefail
segment="${1:?segment required}"
shift
entry_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export ROOST_PERF_REPO="${ROOST_PERF_REPO:-$(cd "$entry_dir/../../../.." && pwd)}"
cd "$ROOST_PERF_REPO"
export GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0
export ROOST_SEGMENT_START_MONO="$(python3 -c 'import time; print(time.monotonic())')"
export ROOST_SEGMENT_DEADLINE_MONO="$(python3 -c 'import os; print(float(os.environ["ROOST_SEGMENT_START_MONO"])+1800)')"
# A read-only plan also validates selection and identifies the new output before up.
plan="$(python3 "$entry_dir/run-micro.py" "$segment" "$@" --plan)"
for arg in "$@"; do
  if [[ "$arg" == --plan ]]; then printf '%s\n' "$plan"; exit 0; fi
done
output="$(printf '%s' "$plan" | python3 -c 'import json,sys;print(json.load(sys.stdin)["output"])')"
[[ ! -e "$output" && ! -e "$output.control" ]] || { echo "output/control already exists: $output" >&2; exit 2; }
control="$output.control"
mkdir -p "$(dirname "$control")"
mkdir "$control"
printf '%s\n' "$plan" >"$control/plan.json"
deps=0
case "$segment" in
  s1-micro|s2-heartbeat|s3-sync|s4-nest-game|s9-demo100|s10-demo200) ;;
  s5-gate-budget|s6-catchup-mail|s7-block-aoi|s8-alliance-effects|b2-footprints|s11-cellworld-robot|s12-gate-pacing|s13-generated-tcp|s14-checkpoint128|s15-checkpoint256) deps=1 ;;
  *) echo "unknown segment: $segment" >&2; exit 2 ;;
esac
export ROOST_MIRROR_LOCAL_HOME="${ROOST_MIRROR_LOCAL_HOME:-/private/tmp/v147-formal-mirror-20261114}"
export ROOST_MIRROR_LOCAL_OFFSET="${ROOST_MIRROR_LOCAL_OFFSET:-18000}"
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if (( deps )); then
    python3 "$entry_dir/timed-command.py" "$ROOST_SEGMENT_DEADLINE_MONO" "$control/down.json" bash scripts/mirror-local.sh down >"$control/dependencies-down.log" 2>&1
    down_rc=$?
    [[ "$down_rc" == 0 ]] || rc="$down_rc"
  fi
  python3 - "$control" "$rc" <<'PY'
import datetime,json,os,pathlib,sys,time
result={'exit':int(sys.argv[2]),'ended_utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'total_seconds_including_dependencies':time.monotonic()-float(os.environ['ROOST_SEGMENT_START_MONO'])}
pathlib.Path(sys.argv[1],'result.json').write_text(json.dumps(result,indent=2)+'\n');print(json.dumps(result))
PY
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if (( deps )); then
  up_deadline="$(python3 -c 'import os; print(float(os.environ["ROOST_SEGMENT_DEADLINE_MONO"])-120)')"
  python3 "$entry_dir/timed-command.py" "$up_deadline" "$control/up.json" bash scripts/mirror-local.sh up >"$control/dependencies-up.log" 2>&1
  source "$ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it/env.sh"
fi
python3 "$entry_dir/run-micro.py" "$segment" "$@"
