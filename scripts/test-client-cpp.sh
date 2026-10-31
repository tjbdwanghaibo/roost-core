#!/usr/bin/env bash
# Native client validation; protobuf tests use an explicitly installed host toolchain.
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"
run_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-native.XXXXXX")"
trap 'rm -rf "$run_dir"' EXIT
shared=OFF
if [[ "${ROOST_MANAGED_TESTS:-0}" == "1" ]]; then
  command -v dotnet >/dev/null || { echo "Required tool not found: dotnet" >&2; exit 1; }
  shared=ON
fi
cmake -S client/cpp -B "$run_dir" -DBUILD_SHARED_LIBS="$shared" -DROOST_WITH_PROTOBUF="${ROOST_WITH_PROTOBUF:-${ROOST_PROTOBUF_TESTS:-OFF}}" -DROOST_BUILD_TESTS=ON -DROOST_PROTOBUF_TESTS="${ROOST_PROTOBUF_TESTS:-OFF}" -DCMAKE_PREFIX_PATH="${ROOST_PROTOBUF_PREFIX:-}"
cmake --build "$run_dir" --parallel 4
ctest --test-dir "$run_dir" --output-on-failure

if [[ "${ROOST_MANAGED_TESTS:-0}" == "1" ]]; then
  dotnet build client/cpp/tests/managed/NativeInterop.csproj --disable-build-servers
  DYLD_LIBRARY_PATH="$run_dir" LD_LIBRARY_PATH="$run_dir" dotnet client/cpp/tests/managed/bin/Debug/net10.0/NativeInterop.dll
  ROOST_NATIVE_LIBRARY_DIR="$run_dir" python3 client/cpp/tests/game.py dotnet client/cpp/tests/managed/bin/Debug/net10.0/NativeInterop.dll
fi
