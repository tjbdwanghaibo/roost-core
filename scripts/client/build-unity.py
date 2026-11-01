#!/usr/bin/env python3
"""Build a relocatable Unity UPM package; never modify a user's Unity project."""
import argparse
import json
import os
import pathlib
import platform
import shutil
import subprocess
import uuid
import hashlib

p = argparse.ArgumentParser()
p.add_argument(
    "--target", choices=["macos", "linux", "windows", "android", "ios"], default="macos"
)
p.add_argument("--output", required=True)
p.add_argument(
    "--merge-plugins-from", help="built package from the same native sources"
)
p.add_argument("--protoc", default=shutil.which("protoc"))
p.add_argument("--arch", choices=["arm64"], default="arm64")
a = p.parse_args()
root = pathlib.Path(__file__).resolve().parents[2]
out = pathlib.Path(a.output).resolve()
if out.exists():
    p.error("output already exists; choose a new directory")
cmake = shutil.which("cmake")
if not cmake:
    p.error("cmake required in PATH")
args = ["-DBUILD_SHARED_LIBS=ON", "-DCMAKE_BUILD_TYPE=Release"]
folder = {
    "macos": "macOS",
    "linux": "Linux",
    "windows": "Windows",
    "android": "Android",
    "ios": "iOS",
}[a.target]
if a.target == "macos":
    if platform.system() != "Darwin":
        p.error("macOS build requires macOS")
    args += ["-DCMAKE_OSX_ARCHITECTURES=" + a.arch]
elif a.target == "android":
    ndk = os.environ.get("ANDROID_NDK_HOME")
    if not ndk:
        p.error("ANDROID_NDK_HOME required")
    args += [
        "-DCMAKE_TOOLCHAIN_FILE=" + ndk + "/build/cmake/android.toolchain.cmake",
        "-DANDROID_ABI=arm64-v8a",
        "-DANDROID_PLATFORM=android-24",
    ]
elif a.target == "ios":
    args += [
        "-G",
        "Xcode",
        "-DCMAKE_SYSTEM_NAME=iOS",
        "-DCMAKE_OSX_ARCHITECTURES=arm64",
        "-DBUILD_SHARED_LIBS=OFF",
    ]
elif (a.target == "windows" and platform.system() != "Windows") or (
    a.target == "linux" and platform.system() != "Linux"
):
    p.error("build this target on its native host")
if a.target in ("windows", "linux") and platform.machine().lower() not in (
    "x86_64",
    "amd64",
):
    p.error("the Unity desktop importer currently supports x86_64 on this target")
if not a.protoc:
    p.error("protoc required for the generated DAO sample")
source_files = sorted((root / "client/cpp/src").glob("*.cpp")) + sorted(
    (root / "client/cpp/include/roost").glob("*")
)
fingerprint = hashlib.sha256(
    b"".join(str(f.relative_to(root)).encode() + f.read_bytes() for f in source_files)
).hexdigest()
if a.merge_plugins_from:
    base = pathlib.Path(a.merge_plugins_from)
    if (
        json.loads((base / "build-info.json").read_text()).get("native_source_sha256")
        != fingerprint
    ):
        p.error("base package native sources do not match; rebuild it first")
out.mkdir(parents=True)
subprocess.run(
    [cmake, "-S", str(root / "client/cpp"), "-B", str(out / "build"), *args], check=True
)
subprocess.run(
    [cmake, "--build", str(out / "build"), "--config", "Release", "--parallel", "4"],
    check=True,
)
subprocess.run(
    [
        "dotnet",
        "build",
        str(root / "client/dotnet/Roost.Client/Roost.Client.csproj"),
        "-c",
        "Release",
        "--disable-build-servers",
    ],
    check=True,
)
package = out / "com.roost.client"
shutil.copytree(root / "client/unity", package)
shutil.copytree(root / "client/cpp/unity", package / "Runtime/Native")
generated = package / "Samples~/NativeSession/Generated"
generated.mkdir()
proto = root / "codegen/internal/dao/testdata/golden/dao_sync.proto"
subprocess.run(
    [
        a.protoc,
        "--proto_path=" + str(proto.parent),
        "--csharp_out=" + str(generated),
        str(proto),
    ],
    check=True,
)
shutil.copy2(proto.parent / "dao_sync_apply.cs", generated / "DaoSyncApply.cs")
shutil.copy2(
    root / "client/dotnet/Roost.Client/bin/Release/netstandard2.1/Roost.Client.dll",
    package / "Runtime/Roost.Client.dll",
)
name = {
    "macos": "libroost_client.dylib",
    "linux": "libroost_client.so",
    "windows": "roost_client.dll",
    "android": "libroost_client.so",
    "ios": "libroost_client.a",
}[a.target]
libs = list((out / "build").rglob(name))
if len(libs) != 1:
    raise RuntimeError("expected one native library: " + repr(libs))
if a.merge_plugins_from:
    shutil.copytree(
        pathlib.Path(a.merge_plugins_from) / "Runtime/Plugins",
        package / "Runtime/Plugins",
    )
plugins = package / "Runtime/Plugins" / folder
plugins.mkdir(parents=True, exist_ok=True)
shutil.copy2(libs[0], plugins / name)
(package / "build-info.json").write_text(
    json.dumps(
        {
            "commit": subprocess.check_output(
                ["git", "-C", str(root), "rev-parse", "HEAD"], text=True
            ).strip(),
            "target": a.target,
            "arch": (
                a.arch
                if a.target in ("macos", "ios", "android")
                else platform.machine()
            ),
            "abi": 2,
            "native_source_sha256": fingerprint,
        },
        indent=2,
    )
    + "\n"
)
# Stable GUIDs preserve scene references across SDK rebuilds. Unity fills importer
# settings on first import; the package's AssetPostprocessor applies CPU rules.
for asset in sorted(package.rglob("*")):
    if asset.name.endswith(".meta"):
        continue
    relative = asset.relative_to(package).as_posix()
    guid = uuid.uuid5(uuid.NAMESPACE_URL, "com.roost.client/" + relative).hex
    text = "fileFormatVersion: 2\nguid: " + guid + "\n"
    if asset.is_dir():
        text += "folderAsset: yes\n"
    pathlib.Path(str(asset) + ".meta").write_text(text)
print("Unity package:", package)
