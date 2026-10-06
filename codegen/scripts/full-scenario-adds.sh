#!/usr/bin/env bash
# The add sequence of the framework-compat "full" scenario, defined once: the
# workflow (.github/workflows/framework-compat.yml) and its local mirror
# (codegen/scripts/source-head-check.sh) both run this file, so the two cannot
# drift apart (A11; ci_full_scenario_test.go in the module root pins it).
#
#   full-scenario-adds.sh <roost binary> [project sync arguments...]
#
# Run it in the root of a project generated with -template game and services
# game,gate. The arguments after the binary go to `roost project sync`
# (-skip-deps for the source-head lane). Every step must succeed: an add that
# fails is a generator defect, so nothing here is allowed to swallow it.
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <roost binary> [project sync arguments...]" >&2
  exit 2
fi
roost="$1"
shift
# The sequence runs before the source-head lane builds its temporary
# workspace; no go.work around the project may take part in it.
export GOWORK=off

"$roost" add access player --service gate
"$roost" add transport tcp
# Player and its lifecycle come from -template game; the scenario builds on
# them rather than adding them a second time.
"$roost" add component Profile --entity Player
"$roost" add dao Player --entity Player
"$roost" add handler RenamePlayer --entity Player --component Profile
"$roost" add protocol RenamePlayer --group game --handler player
"$roost" add endpoint RenamePlayer --handler player
"$roost" add skill Fireball
"$roost" add saga GuildTransfer -service game -steps debit,credit
# A project-owned rpc: game owns it, gate calls it. uses_rpcs is a manifest
# edit, so it goes through sync. awk rather than sed -i, whose flags differ
# between GNU (CI) and BSD (macOS).
"$roost" add rpc Guild -service game
awk '{ print } /^    gate:$/ { print "        uses_rpcs:"; print "            - guild" }' roost.yaml >roost.yaml.tmp
mv roost.yaml.tmp roost.yaml
if ! grep -q '^            - guild$' roost.yaml; then
  echo "full-scenario-adds: no '    gate:' entry in roost.yaml to give uses_rpcs" >&2
  exit 1
fi
"$roost" project sync "$@"
