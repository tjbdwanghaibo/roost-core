#!/usr/bin/env python3
"""Create an isolated Unity acceptance project from a built UPM package."""
import argparse, json, pathlib, shutil

p = argparse.ArgumentParser()
p.add_argument("--package", required=True)
p.add_argument("--project", required=True)
p.add_argument("--protobuf-dll", required=True)
p.add_argument("--unsafe-dll", required=True)
a = p.parse_args()
project = pathlib.Path(a.project).resolve()
package = pathlib.Path(a.package).resolve()
if project.exists():
    p.error("project exists")
(project / "ProjectSettings").mkdir(parents=True)
(project / "ProjectSettings/ProjectVersion.txt").write_text(
    "m_EditorVersion: 6000.3.12f1\n"
)
(project / "Packages").mkdir(parents=True)
(project / "Assets/Editor").mkdir(parents=True)
(project / "Packages/manifest.json").write_text(
    json.dumps(
        {
            "dependencies": {
                "com.roost.client": "file:" + str(package),
                "com.unity.modules.imgui": "1.0.0",
                "com.unity.modules.jsonserialize": "1.0.0",
            }
        }
    )
)
shutil.copytree(package / "Samples~/NativeSession", project / "Assets/NativeSession")
shutil.copy2(a.protobuf_dll, project / "Assets/NativeSession/Google.Protobuf.dll")
shutil.copy2(
    a.unsafe_dll,
    project / "Assets/NativeSession/System.Runtime.CompilerServices.Unsafe.dll",
)
shutil.copy2(
    pathlib.Path(__file__).with_name("UnityAcceptanceBuild.cs"),
    project / "Assets/Editor/UnityAcceptanceBuild.cs",
)
print(project)
