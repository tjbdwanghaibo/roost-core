#!/usr/bin/env sh
# Compile cfggen's output against roost-core's configdata and push real data
# through it.
#
# roost-codegen deliberately has no runtime dependency, so its own test suite
# can only compare generated text and gofmt it. That is the blind spot U-0224
# lived in for the dao generator: the text was exactly what the goldens said,
# and what it persisted was an empty document. cfggen has the same exposure —
# its json tags, key types, index and ref metadata are only ever compared as
# strings. This script is the missing half: generate from
# internal/cfggen/testdata/runtime/cfg.yaml into a throwaway module with
# roost-core (the pin from scripts/source-head-check.sh unless ROOST_CORE_PIN
# says otherwise) and run the roundtrip test against the real runtime.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
core_pin="${ROOST_CORE_PIN:-$(sed -n 's/^core_pin="\${ROOST_CORE_PIN:-\(v[0-9.]*\)}"$/\1/p' "$here/scripts/source-head-check.sh")}"
[ -n "$core_pin" ] || { echo "cannot determine the roost-core pin" >&2; exit 2; }

work=$(mktemp -d "${TMPDIR:-/tmp}/cfggen-golden-runtime.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/cfg/data"

# The generator under test, not a published one.
(cd "$here" && go run ./cmd/cfggen -meta internal/cfggen/testdata/runtime/cfg.yaml -out "$work/cfg" -pkg cfg)

cp "$here"/internal/cfggen/testdata/runtime/data/*.json "$work/cfg/data/"
sed '/^\/\/go:build cfgruntime$/d' "$here/internal/cfggen/testdata/runtime/roundtrip_test.go" > "$work/cfg/roundtrip_test.go"

cat > "$work/go.mod" <<EOF
module cfggenruntime

go 1.27.0

require github.com/tjbdwanghaibo/roost-core $core_pin
EOF

cd "$work"
GOWORK=off GOFLAGS=-mod=mod go mod tidy >/dev/null
GOWORK=off go test ./cfg/ "$@"
