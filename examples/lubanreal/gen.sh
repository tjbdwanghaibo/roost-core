#!/usr/bin/env bash
# 用官方 luban CLI 重新生成 gen/（Go 代码）与 configs/data/（导出数据）。
# 前置：
#   1. 下载 Luban 发布包（https://github.com/focus-creative-games/luban/releases），
#      解压后把 LUBAN_DLL 指向其中的 Luban.dll；
#   2. 安装 .NET 8+（更高版本运行时用 DOTNET_ROLL_FORWARD=LatestMajor 兼容）。
set -euo pipefail
cd "$(dirname "$0")"
: "${LUBAN_DLL:?set LUBAN_DLL to the extracted Luban.dll path}"
DOTNET_ROLL_FORWARD=LatestMajor dotnet "$LUBAN_DLL" \
  -t server -c go-json -d json --conf luban.conf \
  -x outputCodeDir=gen -x outputDataDir=configs/data \
  -x lubanGoModule=github.com/tjbdwanghaibo/roost-core/examples/lubanreal/luban
gofmt -w gen
