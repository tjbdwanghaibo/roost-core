#!/usr/bin/env bash
set -euo pipefail
source_root="$1"
bench_root="$2"
benchmark_source="$3"
if [[ -e "$bench_root/go.mod" ]]; then
  echo "Use a new benchmark directory" >&2
  exit 1
fi
mkdir -p "$bench_root/gen" "$bench_root/rangecontract"
cp "$source_root"/codegen/internal/dao/testdata/golden/*.go "$bench_root/gen/"
rm -f "$bench_root"/gen/gen_*_redis_dao.go
for source_test in "$source_root"/codegen/internal/dao/testdata/runtime/*_test.go; do
 sed '/^\/\/go:build daoruntime$/d' "$source_test" > "$bench_root/gen/$(basename "$source_test")"
done
sed '/^\/\/go:build daoruntime$/d' "$benchmark_source" > "$bench_root/gen/persist_paths_bench_test.go"
cp "$source_root/internal/rangecontract/rangecontract.go" "$bench_root/rangecontract/"
cd "$bench_root"
GOWORK=off go mod init daogoldenruntime
GOWORK=off go mod edit -require github.com/tjbdwanghaibo/roost-core@v0.0.0 -replace "github.com/tjbdwanghaibo/roost-core=$source_root"
GOWORK=off go mod tidy
GOWORK=off go test -c -o "$bench_root/bench" ./gen
