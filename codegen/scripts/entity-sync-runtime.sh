#!/usr/bin/env sh
# Compile a generated sync=true entity against roost-core and build it.
#
# roost-codegen has no runtime dependency, so `go test ./internal/entity` can
# only compare generated text. That is how the sync wiring drifted: the
# generated file set `FlushPolicy` and `SubjectPackerFactory` on a struct that
# has neither, and named a constant Core removed — every text test stayed
# green while no project with sync=true could build (RR-20260918-01). This
# script generates from internal/entity/testdata/syncruntime into a throwaway
# module with roost-core (the pin from scripts/source-head-check.sh unless
# ROOST_CORE_PIN says otherwise) and runs the roundtrip test there.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
core_pin="${ROOST_CORE_PIN:-$(sed -n 's/^core_pin="\${ROOST_CORE_PIN:-\(v[0-9.]*\)}"$/\1/p' "$here/scripts/source-head-check.sh")}"
[ -n "$core_pin" ] || { echo "cannot determine the roost-core pin" >&2; exit 2; }

work=$(mktemp -d "${TMPDIR:-/tmp}/entity-sync-runtime.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/syncruntime"

cp "$here/internal/entity/testdata/syncruntime/entity.go" "$work/syncruntime/"
sed '/^\/\/go:build entitysyncruntime$/d' "$here/internal/entity/testdata/syncruntime/roundtrip_test.go" > "$work/syncruntime/roundtrip_test.go"

# The generator under test, not a published one.
(cd "$here" && go run ./cmd/entity -dir "$work/syncruntime" -force)

cat > "$work/go.mod" <<EOF
module entitysyncruntime

go 1.27.0

require github.com/tjbdwanghaibo/roost-core $core_pin
EOF

cd "$work"
GOWORK=off GOFLAGS=-mod=mod go mod tidy >/dev/null
GOWORK=off go test ./syncruntime/ "$@"
