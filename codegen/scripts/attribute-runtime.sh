#!/usr/bin/env sh
# Compile the attribute generator's output against the framework contract and
# run it.
#
# The attribute feature ships two halves: roost-core/attribute (the ids,
# metadata, profile interface, selector, snapshot and container) and this
# generator (one project profile's ids, masks, typed setters, derived formula
# and typed accessors). Nothing checked that the two fit: codegen's tests
# compare text, and the scaffold produced an empty package, so `roost add
# attribute` plus a profile generated code that referenced types nobody
# defined (RR-20260917-06). This script puts the scaffold's runtime file, a
# real profile and the generated output in a throwaway module with the pinned
# roost-core and runs the roundtrip test.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
core_pin="${ROOST_CORE_PIN:-$(sed -n 's/^core_pin="\${ROOST_CORE_PIN:-\(v[0-9.]*\)}"$/\1/p' "$here/scripts/source-head-check.sh")}"
[ -n "$core_pin" ] || { echo "cannot determine the roost-core pin" >&2; exit 2; }

work=$(mktemp -d "${TMPDIR:-/tmp}/attribute-runtime.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/combat"

cp "$here/internal/attribute/testdata/runtime/profile.go" "$work/combat/"
sed '/^\/\/go:build attributeruntime$/d' "$here/internal/attribute/testdata/runtime/roundtrip_test.go" > "$work/combat/roundtrip_test.go"

# The scaffold's runtime file is what a project with the attribute feature
# gets; render it from the same source the scaffold uses.
(cd "$here" && go run ./internal/roost/cmd/attributeruntime combat) > "$work/combat/runtime.go"

(cd "$here" && go run ./cmd/attribute -dir "$work/combat")

cat > "$work/go.mod" <<EOF
module attributeruntime

go 1.27.0

require github.com/tjbdwanghaibo/roost-core $core_pin
EOF

cd "$work"
# roost-core/attribute is newer than the pinned release while this feature is
# being landed: point at the local checkout when it has the package and the
# pin does not. ROOST_CORE_DIR overrides the location.
core_dir="${ROOST_CORE_DIR:-$here/../roost-core}"
if [ -d "$core_dir/attribute" ]; then
	cat >> "$work/go.mod" <<EOF

replace github.com/tjbdwanghaibo/roost-core => $core_dir
EOF
fi
if ! GOWORK=off GOFLAGS=-mod=mod go mod tidy >/dev/null 2>"$work/tidy.err"; then
	# roost-core/attribute lands one release before the generator half can
	# depend on it. Until the pin carries the package there is nothing to
	# gate, and saying so beats a red build that no change can fix.
	if grep -q "roost-core/attribute" "$work/tidy.err"; then
		echo "attribute-runtime: roost-core $core_pin has no attribute package yet; skipping"
		exit 0
	fi
	cat "$work/tidy.err" >&2
	exit 1
fi
GOWORK=off go test ./combat/ "$@"
