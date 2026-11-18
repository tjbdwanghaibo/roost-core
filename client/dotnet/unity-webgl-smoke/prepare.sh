#!/usr/bin/env bash
# 把冒烟工程拷到仓库外的新目录，并放进 scripts/client/build-unity.py --managed-only 打出的 Unity 托管包
# （Roost.Client.dll、Roost.Client.Relay.dll、RoostClientDriver、RoostPlatform 与 WebGL 浏览器桥接；放在 <目录>/RoostSdk，
# Packages/manifest.json 以 file: 引用它）。冒烟工程自己不带桥接，GateSmoke 与 RelaySmoke 都用包里的，与游戏工程相同。
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
mkdir -p "$dest"
cp -R "$here/Assets" "$here/Packages" "$here/ProjectSettings" "$dest/"
python3 "$repo/scripts/client/build-unity.py" --managed-only --output "$dest/RoostSdk" >/dev/null
python3 - "$dest/Packages/manifest.json" <<'PY'
import json, sys
path = sys.argv[1]
manifest = json.load(open(path))
manifest["dependencies"]["com.roost.client"] = "file:../RoostSdk/com.roost.client"
json.dump(manifest, open(path, "w"), indent=2)
PY
echo "$dest"
