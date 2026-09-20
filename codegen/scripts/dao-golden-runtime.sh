#!/usr/bin/env sh
# Compile the dao generator's goldens against roost-core and the Mongo driver
# and push them through the real codec.
#
# roost-codegen deliberately has no runtime dependency, so its own test suite
# can only compare generated text. That is how U-0224 stayed invisible: the
# nested structs it generated were byte-for-byte what the goldens said, and
# what they persisted was {"dirtyhook": {}}. This script is the missing half:
# a throwaway module with the goldens as a package, roost-core (the pin from
# scripts/source-head-check.sh unless ROOST_CORE_PIN says otherwise) and the
# driver, running internal/dao/testdata/runtime/roundtrip_test.go.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
core_pin="${ROOST_CORE_PIN:-$(sed -n 's/^core_pin="\${ROOST_CORE_PIN:-\(v[0-9.]*\)}"$/\1/p' "$here/scripts/source-head-check.sh")}"
[ -n "$core_pin" ] || { echo "cannot determine the roost-core pin" >&2; exit 2; }

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

cd "$work"
GOWORK=off go mod init daogoldenruntime >/dev/null
GOWORK=off GOFLAGS=-mod=mod GONOSUMDB=github.com/tjbdwanghaibo go get "github.com/tjbdwanghaibo/roost-core@$core_pin" >/dev/null
GOWORK=off GOFLAGS=-mod=mod go mod tidy >/dev/null
echo "dao golden runtime: roost-core $core_pin"
GOWORK=off go vet ./...
GOWORK=off go test -count=1 ./...
