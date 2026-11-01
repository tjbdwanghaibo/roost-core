#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
project=${1:-"$(dirname "$root")/roost-physics-viewer"}
unity=${UNITY_EDITOR:-/Applications/Unity/Hub/Editor/6000.3.12f1/Unity.app/Contents/MacOS/Unity}
if [[ -e "$project" ]]; then
 echo "Refusing to overwrite existing project: $project" >&2; exit 1
fi
mkdir -p "$project/Assets/Roost/Editor" "$project/Assets/Roost/Client" "$project/Packages" "$project/ProjectSettings"
mkdir -p "$project/Assets/Roost/Resources"
cp "$root/gameplay/physics/terrain.json" "$project/Assets/Roost/Resources/PhysicsTerrain.json"
cp "$root/client/physics-viewer/"*.cs "$project/Assets/Roost/"
cp "$root/client/physics-viewer/Editor/ViewerProject.cs" "$project/Assets/Roost/Editor/"
cp "$root/client/dotnet/Roost.Client/Packet.cs" "$project/Assets/Roost/Client/"
cp "$root/client/dotnet/Roost.Client/PredictionHistory.cs" "$project/Assets/Roost/Client/"
cp "$root/client/dotnet/Roost.Client/SnapshotBuffer.cs" "$project/Assets/Roost/Client/"
cp "$root/client/dotnet/Roost.Client/NetworkClock.cs" "$project/Assets/Roost/Client/"
printf '%s\n' 'm_EditorVersion: 6000.3.12f1' > "$project/ProjectSettings/ProjectVersion.txt"
printf '%s\n' '{"dependencies":{"com.unity.modules.physics":"1.0.0","com.unity.modules.imgui":"1.0.0","com.unity.modules.jsonserialize":"1.0.0"}}' > "$project/Packages/manifest.json"
printf '%s\n' '/Library/' '/Temp/' '/Logs/' '/Obj/' '/UserSettings/' '/Builds/' '/Artifacts/' '*.log' > "$project/.gitignore"
"$unity" -batchmode -nographics -quit -projectPath "$project" -executeMethod ViewerProject.CreateScene -logFile "$project/create.log"
