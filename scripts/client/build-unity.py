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
p.add_argument(
    "--managed-only",
    action="store_true",
    help="托管路线只要 Roost.Client.dll 与 RoostClientDriver：不构建 native 插件、不生成原生示例，只需要 .NET SDK",
)
a = p.parse_args()
root = pathlib.Path(__file__).resolve().parents[2]
out = pathlib.Path(a.output).resolve()
if out.exists():
    p.error("output already exists; choose a new directory")
cmake = None if a.managed_only else shutil.which("cmake")
if not a.managed_only and not cmake:
    p.error("cmake required in PATH (the managed route needs only --managed-only and the .NET SDK)")
if a.managed_only and a.merge_plugins_from:
    p.error("--merge-plugins-from needs a native build; drop --managed-only")
args = ["-DBUILD_SHARED_LIBS=ON", "-DCMAKE_BUILD_TYPE=Release"]
folder = {
    "macos": "macOS",
    "linux": "Linux",
    "windows": "Windows",
    "android": "Android",
    "ios": "iOS",
}[a.target]
if a.managed_only:
    pass
elif a.target == "macos":
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
if not a.managed_only and a.target in ("windows", "linux") and platform.machine().lower() not in (
    "x86_64",
    "amd64",
):
    p.error("the Unity desktop importer currently supports x86_64 on this target")
if not a.managed_only and not a.protoc:
    p.error("protoc required for the generated DAO sample")
# src 下的 .hpp 是库内部头文件（如 auth_payload.hpp），改动同样改变插件行为，一并计入指纹。
source_files = sorted(
    f for f in (root / "client/cpp/src").glob("*") if f.suffix in (".cpp", ".hpp")
) + sorted((root / "client/cpp/include/roost").glob("*"))
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
if not a.managed_only:
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
# 原生绑定（Runtime/Native）只是 C#，托管包也带上：RoostNativeDriver 引用它，缺了 Roost.Unity 编译不过；没有插件时只在真用原生路线时报找不到库。
if a.managed_only:
    shutil.rmtree(package / "Samples~/NativeSession")  # 原生示例要插件和生成的 DAO，托管包不带
    manifest = json.loads((package / "package.json").read_text())
    manifest.pop("samples", None)
    (package / "package.json").write_text(json.dumps(manifest, indent=2) + "\n")
generated = package / "Samples~/NativeSession/Generated"
if not a.managed_only:
    generated.mkdir()
proto = root / "codegen/internal/dao/testdata/golden/dao_sync.proto"
if not a.managed_only:
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
libs = [] if a.managed_only else list((out / "build").rglob(name))
if not a.managed_only and len(libs) != 1:
    raise RuntimeError("expected one native library: " + repr(libs))
if a.merge_plugins_from:
    shutil.copytree(
        pathlib.Path(a.merge_plugins_from) / "Runtime/Plugins",
        package / "Runtime/Plugins",
    )
if not a.managed_only:
    plugins = package / "Runtime/Plugins" / folder
    plugins.mkdir(parents=True, exist_ok=True)
    shutil.copy2(libs[0], plugins / name)
(package / "build-info.json").write_text(
    json.dumps(
        {
            "commit": subprocess.check_output(
                ["git", "-C", str(root), "rev-parse", "HEAD"], text=True
            ).strip(),
            "target": "managed" if a.managed_only else a.target,
            "arch": (
                None
                if a.managed_only
                else a.arch
                if a.target in ("macos", "ios", "android")
                else platform.machine()
            ),
            "abi": 2,
            "native_source_sha256": None if a.managed_only else fingerprint,
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
