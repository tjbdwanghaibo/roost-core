#!/usr/bin/env bash
# 把冒烟工程拷到仓库外的新目录，并放进刚编好的 Roost.Client.Relay.dll（netstandard2.1，Release）。
# Unity 打开工程会生成 Library/、.meta 等，所以不在仓库里直接打开。
#   client/dotnet/unity-webgl-smoke/prepare.sh /tmp/roost-relay-smoke
set -euo pipefail
dest="${1:?usage: prepare.sh <new project directory>}"
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
if [[ -e "$dest" ]]; then
  echo "prepare.sh: $dest already exists; pass a new directory" >&2
  exit 1
fi
dotnet build "$repo/client/dotnet/Roost.Client.Relay/Roost.Client.Relay.csproj" -c Release -nologo -v quiet
mkdir -p "$dest"
cp -R "$here/Assets" "$here/Packages" "$here/ProjectSettings" "$dest/"
mkdir -p "$dest/Assets/Plugins"
cp "$repo/client/dotnet/Roost.Client.Relay/bin/Release/netstandard2.1/Roost.Client.Relay.dll" "$dest/Assets/Plugins/"
echo "$dest"
