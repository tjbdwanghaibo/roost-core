#!/usr/bin/env bash
# C# 客户端 SDK 的验证：金样（client/spec/packets.json）往返、会话 / 推送 / Sync 恢复 / Lockstep 追帧与快照恢复 / Unity 适配
# 自测，地形 SDK 的金样与自测（client/dotnet/Roost.Client.Terrain.Tests），跨 Gate 放弃握手（infra/network/gateway 的
# C# 客户端用例），以及真实生成工程上的 TCP 鉴权 → PB → Sync 推送 → Lockstep 追帧（TestClientSDKAgainstGeneratedPlayerTCP）。
#
# 何时跑：改协议、client/wire、framework/sync/frame、entitysync 帧、lockstep 编码、client/dotnet、gameplay/terrain 的包格式 /
# 查询算法或 DAO 同步生成器时；pretag 与 CI 的
# client-dotnet job 也会跑。没有 dotnet 时明确打印 SKIP 并以 0 退出（本机可以不装 .NET）；设 ROOST_REQUIRE_DOTNET=1
# 时缺 dotnet 以 1 退出（CI 用，不允许静默跳过）。ROOST_DOTNET_GOLDEN_ONLY=1 只跑金样与自测（pretag 用：生成工程
# 那一条已在 go test ./... 里随 dotnet 可用而运行；地形 DAO 夹具的漂移核对也只在完整模式跑）。
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

# 地形 SDK（Roost.Client/Terrain）：Go 金样 terrain-golden.pack + terrain-queries.json 逐点一致、坏包 / 坏块、覆盖块规则、
# mesh 网格接缝，以及 Go 生成 DAO 写出的 TerrainChunk 全量 / 增量经 C# 生成 Apply 后的查询。几秒内结束，pretag 也跑。
echo "test-client-dotnet: terrain SDK golden + self tests"
dotnet run --project client/dotnet/Roost.Client.Terrain.Tests -- client/spec

# 跨 Gate 放弃握手：同一进程内两个独立 Gate + 一个 Game，真实 C# 客户端放弃 Gate A 上的握手、转连 Gate B，
# 旧 Bind 迟到完成时新连接不能被断开（鉴权首包的连接代号）。用上一步 dotnet run 构建的测试程序，几秒内结束，
# pretag 的 ROOST_DOTNET_GOLDEN_ONLY 也跑。
echo "test-client-dotnet: cross-gate abandoned handshake with the C# client"
ROOST_CLIENT_TEST_DLL="$repo_dir/client/dotnet/Roost.Client.Tests/bin/Debug/net10.0/Roost.Client.Tests.dll" \
  GOWORK=off go test -count=1 -run '^TestAcrossGatesTheCSharpClientThatAbandonsAHandshakeKeepsItsNewerConnection$' ./infra/network/gateway

if [[ "${ROOST_DOTNET_GOLDEN_ONLY:-}" == "1" ]]; then
  exit 0
fi
# 地形测试用的 TerrainChunk 生成物与 Go 同步夹具必须与当前生成器 / DAO 定义 / gameplay/terrain 一致（临时 module，离线）。
echo "test-client-dotnet: terrain DAO fixture is current"
sh client/dotnet/Roost.Client.Terrain.Tests/Fixtures/regen.sh

echo "test-client-dotnet: generated player TCP project"
GOWORK=off go test -count=1 -timeout 10m -run '^TestClientSDKAgainstGeneratedPlayerTCP$' ./codegen/internal/roost
