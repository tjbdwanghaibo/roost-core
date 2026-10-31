#!/usr/bin/env bash
# Native client validation; protobuf tests use an explicitly installed host toolchain.
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"
run_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-native.XXXXXX")"
trap 'rm -rf "$run_dir"' EXIT
cmake -S client/cpp -B "$run_dir" -DROOST_BUILD_TESTS=ON -DROOST_PROTOBUF_TESTS="${ROOST_PROTOBUF_TESTS:-OFF}" -DCMAKE_PREFIX_PATH="${ROOST_PROTOBUF_PREFIX:-}"
cmake --build "$run_dir" --parallel 4
ctest --test-dir "$run_dir" --output-on-failure
