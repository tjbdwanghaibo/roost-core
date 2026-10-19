#!/usr/bin/env bash
# C# 客户端 SDK 的验证：金样（client/spec/packets.json）往返、会话 / 推送 / Sync 恢复 / Lockstep 追帧 / Unity 适配
# 自测，以及真实生成工程上的 TCP 鉴权 → PB → Sync 推送 → Lockstep 追帧（TestClientSDKAgainstGeneratedPlayerTCP）。
#
# 何时跑：改协议、client/wire、framework/sync/frame、entitysync 帧、lockstep 编码或 client/dotnet 时；pretag 与 CI 的
# client-dotnet job 也会跑。没有 dotnet 时明确打印 SKIP 并以 0 退出（本机可以不装 .NET）；设 ROOST_REQUIRE_DOTNET=1
# 时缺 dotnet 以 1 退出（CI 用，不允许静默跳过）。ROOST_DOTNET_GOLDEN_ONLY=1 只跑金样与自测（pretag 用：生成工程
# 那一条已在 go test ./... 里随 dotnet 可用而运行）。
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"

if ! command -v dotnet >/dev/null 2>&1; then
  if [[ "${ROOST_REQUIRE_DOTNET:-}" == "1" ]]; then
    echo "test-client-dotnet: dotnet not found and ROOST_REQUIRE_DOTNET=1" >&2
    exit 1
  fi
  echo "test-client-dotnet: SKIP C# client SDK tests: dotnet not found (install the .NET 10 SDK to run them); this is not a C# pass" >&2
  exit 0
fi

export DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1
echo "test-client-dotnet: golden + SDK self tests ($(dotnet --version))"
dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json

if [[ "${ROOST_DOTNET_GOLDEN_ONLY:-}" == "1" ]]; then
  exit 0
fi
echo "test-client-dotnet: generated player TCP project"
GOWORK=off go test -count=1 -timeout 10m -run '^TestClientSDKAgainstGeneratedPlayerTCP$' ./codegen/internal/roost
