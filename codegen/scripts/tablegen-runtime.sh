#!/usr/bin/env sh
# Compile tablegen's generated loader against roost-core's configdata and push
# real JSON through it — the tablegen counterpart of cfggen-golden-runtime.sh.
# tablegen's own tests compare text; whether a declared rule (ref=) is enforced
# when the data is loaded is only visible against the real runtime
# (RR-20261005-NC-75). The fixture is internal/tablegen/testdata/runtime; the
# pinned roost-core is the generator's minimum from scripts/core-pin.sh unless
# ROOST_CORE_PIN says otherwise. ROOST_CORE_DIR=<checkout> replaces the pin with
# a local roost-core checkout, for a change that lands in configdata and the
# generator together before the pin can move (B10).
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
core_pin=$(sh "$here/scripts/core-pin.sh")

work=$(mktemp -d "${TMPDIR:-/tmp}/tablegen-runtime.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/configs/schema" "$work/configs/generated/data"

cat > "$work/go.mod" <<MOD
module tablegenruntime

go 1.27.0

require github.com/tjbdwanghaibo/roost-core $core_pin
MOD
if [ -n "${ROOST_CORE_DIR:-}" ]; then
	echo "replace github.com/tjbdwanghaibo/roost-core => $ROOST_CORE_DIR" >> "$work/go.mod"
fi
cp "$here"/internal/tablegen/testdata/runtime/schema/*.go "$work/configs/schema/"

# The generator under test, not a published one.
(cd "$here" && go run ./cmd/tablegen -meta "$work/configs/schema" -out "$work/configs/generated" -pkg generated -force)

cp "$here"/internal/tablegen/testdata/runtime/data/*.json "$work/configs/generated/data/"
sed '/^\/\/go:build tablegenruntime$/d' "$here/internal/tablegen/testdata/runtime/roundtrip_test.go" > "$work/configs/generated/roundtrip_test.go"

cd "$work"
GOWORK=off GOFLAGS=-mod=mod go mod tidy >/dev/null
GOWORK=off go test ./configs/generated/ "$@"
