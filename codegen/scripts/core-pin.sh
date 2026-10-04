#!/usr/bin/env sh
# Print the roost-core version the codegen runtime guards (*-runtime.sh)
# compile the generator's output against. ROOST_CORE_PIN overrides it.
#
# The pin is minimumVersions.Core in internal/roost/manifest.go — "the lowest
# framework this generator's output compiles against". That is the promise
# the guards test: every project the generator writes may resolve that
# release, so the goldens, the scaffold's runtime files and the generated
# entities must build and run there. It is also always a published tag.
#
# Not codegen/ci/framework-release.yaml's `release:`: scripts/pretag.sh makes
# it name the version being released BEFORE that tag exists, so on every
# release commit the guards would fail to download their own pin.
#
# RR-20260921-05: the guards used to sed core_pin="${ROOST_CORE_PIN:-vX}" out
# of scripts/source-head-check.sh; 6d04aea4 removed that line when the release
# chain became one module, and all four exited 2 ("cannot determine the
# roost-core pin") without anybody noticing — no workflow ran them.
# internal/roost/core_pin_script_promises_test.go pins this script's output to
# minimumVersions.Core.
set -eu

if [ -n "${ROOST_CORE_PIN:-}" ]; then
	echo "$ROOST_CORE_PIN"
	exit 0
fi

manifest="$(cd "$(dirname "$0")/.." && pwd)/internal/roost/manifest.go"
pin=$(awk '
	/^var minimumVersions = VersionSpec[{]/ { inside = 1; next }
	inside && /^}/ { exit }
	inside && $1 == "Core:" { gsub(/[",]/, "", $2); print $2; exit }
' "$manifest")
case "$pin" in
v[0-9]*.[0-9]*.[0-9]*) echo "$pin" ;;
*)
	echo "core-pin: cannot read minimumVersions.Core from $manifest (got '$pin'); set ROOST_CORE_PIN" >&2
	exit 2
	;;
esac
