#!/usr/bin/env sh
# Compile the dao generator's goldens against roost-core and the Mongo driver
# and push them through the real codec.
#
# roost-codegen deliberately has no runtime dependency, so its own test suite
# can only compare generated text. That is how U-0224 stayed invisible: the
# nested structs it generated were byte-for-byte what the goldens said, and
# what they persisted was {"dirtyhook": {}}. This script is the missing half:
# a throwaway module with the goldens as a package, roost-core (the
# generator's minimum from scripts/core-pin.sh unless ROOST_CORE_PIN says
# otherwise) and the driver, running
# internal/dao/testdata/runtime/roundtrip_test.go.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
# The generator's minimum roost-core unless ROOST_CORE_PIN says otherwise;
# core-pin.sh says why that is the pin (RR-20260921-05). ROOST_CORE_REPLACE=<checkout>
# compiles against a local roost-core instead (a replace): the way to run the
# goldens before the release that raises the minimum, when the generated code
# uses a framework API that is not published yet. (Not ROOST_CORE_DIR like the
# other guards: dataengine-env.sh calls this script and must not name that
# variable, see TestFaultMatrixScriptNamesEveryFullEnvironmentSuite.)
core_pin=$(sh "$here/scripts/core-pin.sh")

work=$(mktemp -d "${TMPDIR:-/tmp}/dao-golden-runtime.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/gen"
cp "$here"/internal/dao/testdata/golden/*.go "$work/gen/"
# The redis DAO goldens reference the fixture's definition types (CacheSession,
# RawSession), which are not generated; they are text-only goldens here.
rm -f "$work"/gen/gen_*_redis_dao.go
for t in "$here"/internal/dao/testdata/runtime/*_test.go; do
	sed '/^\/\/go:build daoruntime$/d' "$t" > "$work/gen/$(basename "$t")"
done
# The shared Range contract helper (C7) lives in roost-core's internal/ and
# cannot be imported from another module; range_contract_test.go imports this
# source copy instead.
mkdir -p "$work/rangecontract"
cp "$here/../internal/rangecontract/rangecontract.go" "$work/rangecontract/"

cd "$work"
GOWORK=off go mod init daogoldenruntime >/dev/null
if [ -n "${ROOST_CORE_REPLACE:-}" ]; then
	core_dir=$(cd "$ROOST_CORE_REPLACE" && pwd)
	GOWORK=off go mod edit -require "github.com/tjbdwanghaibo/roost-core@v0.0.0" -replace "github.com/tjbdwanghaibo/roost-core=$core_dir"
	core_pin="local $core_dir"
else
	GOWORK=off GOFLAGS=-mod=mod GONOSUMDB=github.com/tjbdwanghaibo go get "github.com/tjbdwanghaibo/roost-core@$core_pin" >/dev/null
fi
GOWORK=off GOFLAGS=-mod=mod go mod tidy >/dev/null
echo "dao golden runtime: roost-core $core_pin"
GOWORK=off go vet ./...
# stored_keys_mongo_test.go runs on the isolated replica set when
# ROOST_DATAENGINE_IT_MONGO_URI is set and skips otherwise.
GOWORK=off go test -count=1 -run '^Test' ./...
